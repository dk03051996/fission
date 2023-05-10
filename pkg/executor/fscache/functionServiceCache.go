/*
Copyright 2016 The Fission Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package fscache

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	uuid "github.com/satori/go.uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.uber.org/zap"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	fv1 "github.com/fission/fission/pkg/apis/core/v1"
	"github.com/fission/fission/pkg/cache"
	"github.com/fission/fission/pkg/crd"
	ferror "github.com/fission/fission/pkg/error"
	"github.com/fission/fission/pkg/executor/metrics"
)

type fscRequestType int

//type executorType int

// FunctionServiceCache Request Types
const (
	TOUCH fscRequestType = iota
	LISTOLD
	LOG
	LISTOLDPOOL
)

const (
	path     string = "/tmp"
	fileName string = "dump"
)

type (
	// FuncSvc represents a function service
	FuncSvc struct {
		Name              string                  // Name of object
		Function          *metav1.ObjectMeta      // function this pod/service is for
		Environment       *fv1.Environment        // function's environment
		Address           string                  // Host:Port or IP:Port that the function's service can be reached at.
		KubernetesObjects []apiv1.ObjectReference // Kubernetes Objects (within the function namespace)
		Executor          fv1.ExecutorType
		CPULimit          resource.Quantity

		Ctime time.Time
		Atime time.Time
	}

	// FunctionServiceCache represents the function service cache
	FunctionServiceCache struct {
		logger            *zap.Logger
		byFunction        *cache.Cache // function-key -> funcSvc  : map[string]*funcSvc
		byAddress         *cache.Cache // address      -> function : map[string]metav1.ObjectMeta
		byFunctionUID     *cache.Cache // function uid -> function : map[string]metav1.ObjectMeta
		connFunctionCache *PoolCache   // function-key -> funcSvc : map[string]*funcSvc
		PodToFsvc         sync.Map     // pod-name -> funcSvc: map[string]*FuncSvc
		WebsocketFsvc     sync.Map     // funcSvc-name -> bool: map[string]bool
		requestChannel    chan *fscRequest
	}

	fscRequest struct {
		requestType     fscRequestType
		address         string
		age             time.Duration
		responseChannel chan *fscResponse
	}

	fscResponse struct {
		objects []*FuncSvc
		error
	}
)

// IsNotFoundError checks if err is ErrorNotFound.
func IsNotFoundError(err error) bool {
	if fe, ok := err.(ferror.Error); ok {
		return fe.Code == ferror.ErrorNotFound
	}
	return false
}

// IsNameExistError checks if err is ErrorNameExists.
func IsNameExistError(err error) bool {
	if fe, ok := err.(ferror.Error); ok {
		return fe.Code == ferror.ErrorNameExists
	}
	return false
}

// MakeFunctionServiceCache starts and returns an instance of FunctionServiceCache.
func MakeFunctionServiceCache(logger *zap.Logger) *FunctionServiceCache {
	fsc := &FunctionServiceCache{
		logger:            logger.Named("function_service_cache"),
		byFunction:        cache.MakeCache(0, 0),
		byAddress:         cache.MakeCache(0, 0),
		byFunctionUID:     cache.MakeCache(0, 0),
		connFunctionCache: NewPoolCache(logger.Named("conn_function_cache")),
		requestChannel:    make(chan *fscRequest),
	}
	go fsc.service()
	return fsc
}

func (fsc *FunctionServiceCache) service() {
	for {
		req := <-fsc.requestChannel
		resp := &fscResponse{}
		switch req.requestType {
		case TOUCH:
			// update atime for this function svc
			resp.error = fsc._touchByAddress(req.address)
		case LISTOLD:
			// get svcs idle for > req.age
			fscs := fsc.byFunctionUID.Copy()
			funcObjects := make([]*FuncSvc, 0)
			for _, funcSvc := range fscs {
				mI := funcSvc.(metav1.ObjectMeta)
				fsvcI, err := fsc.byFunction.Get(crd.CacheKey(&mI))
				if err != nil {
					fsc.logger.Error("error while getting service", zap.Any("error", err))
					return
				}
				fsvc := fsvcI.(*FuncSvc)
				if time.Since(fsvc.Atime) > req.age {
					funcObjects = append(funcObjects, fsvc)
				}
			}
			resp.objects = funcObjects
		case LOG:
			fsc.logger.Info("dumping function service cache")
			funcCopy := fsc.byFunction.Copy()
			info := []string{}
			for key, fsvcI := range funcCopy {
				fsvc := fsvcI.(*FuncSvc)
				for _, kubeObj := range fsvc.KubernetesObjects {
					info = append(info, fmt.Sprintf("%v\t%v\t%v", key, kubeObj.Kind, kubeObj.Name))
				}
			}
			fsc.logger.Info("function service cache", zap.Int("item_count", len(funcCopy)), zap.Strings("cache", info))
		case LISTOLDPOOL:
			fscs := fsc.connFunctionCache.ListAvailableValue()
			funcObjects := make([]*FuncSvc, 0)
			for _, fsvc := range fscs {
				if time.Since(fsvc.Atime) > req.age {
					funcObjects = append(funcObjects, fsvc)
				}
			}
			resp.objects = funcObjects

		}
		req.responseChannel <- resp
	}
}

// DumpFnSvcCache => dump function service cache data to /tmp directory of executor pod.
func (fsc *FunctionServiceCache) DumpFnSvcCache(ctx context.Context) error {
	fsc.logger.Info("dumping function service group cache")
	fnSvcGroup := fsc.connFunctionCache.GetFnSvcGroup(ctx)

	if len(fnSvcGroup) == 0 {
		return ferror.MakeError(ferror.ErrorNotFound, "function service not found")
	}
	fsc.logger.Debug("dump func svc", zap.Int("lenght", len(fnSvcGroup)), zap.Any("fn svc", fnSvcGroup))
	info := []string{}
	for _, fnSvcGrp := range fnSvcGroup {
		data := fmt.Sprintf("svc_waiting:%d\tqueue_len:%d", fnSvcGrp.svcWaiting, fnSvcGrp.queue.Len())
		fsc.logger.Debug("inside fnSvcGroup", zap.Any("func svc", fnSvcGrp), zap.Int("svc_wait", fnSvcGrp.svcWaiting), zap.Int("len_queue", fnSvcGrp.queue.Len()))
		for addr, fnSvc := range fnSvcGrp.svcs {
			// info = append(info, fmt.Sprintf("function_name:%s\tfn_svc_address:%s\tactive_req:%d\tsvc_waiting:%d\tqueue_len:%d\tcurrent_cpu_usage:%v\tcpu_limit:%v",
			// 	fnSvc.val.Function.Name, addr, fnSvc.activeRequests, fnSvcGrp.svcWaiting, fnSvcGrp.queue.Len(), fnSvc.currentCPUUsage, fnSvc.cpuLimit))
			data = fmt.Sprintf("%s\tfunction_name:%s\tfn_svc_address:%s\tcurrent_cpu_usage:%v\tcpu_limit:%v",
				data, fnSvc.val.Function.Name, addr, fnSvc.currentCPUUsage, fnSvc.cpuLimit)
			fsc.logger.Debug("dump data info", zap.Any("data info", info))
		}
		info = append(info, data)
	}

	fsc.logger.Debug("dump data", zap.Any("data", info))

	if err := dumpData(info, fsc.logger); err != nil {
		fsc.logger.Error("error while dumping function service group cache", zap.Error(err))
		return err
	}

	fsc.logger.Info("dumped function service group cache")
	return nil
}

// check whether /tmp dir exists or not
// if not exists then create /tmp dir and then dump data to a file, if exists then dump data directly to a file
// new file will be created on every request with unique id
func dumpData(data []string, logger *zap.Logger) error {
	logger.Debug("started dumping data")
	writeData := func(data []string) error {
		uid, err := uuid.NewV4()
		if err != nil {
			return ferror.MakeError(ferror.ErrorInternal, fmt.Sprintf("error while generating UID %s", err.Error()))
		}
		// always create a new file with random id under /tmp directory => /tmp/dump_718b5b6e.txt
		file, err := os.Create(fmt.Sprintf("%s/%s_%s.txt", path, fileName, strings.Split(uid.String(), "-")[0]))
		if err != nil {
			return ferror.MakeError(ferror.ErrorInternal, fmt.Sprintf("error while creating file %s", err.Error()))
		}

		datawriter := bufio.NewWriter(file)

		for _, str := range data {
			_, _ = datawriter.WriteString(str + "\n")
		}

		datawriter.Flush()
		file.Close()
		return nil
	}

	// check whether /tmp dir exists or not
	_, err := os.Stat(path)
	if err == nil {
		return writeData(data)
	}
	if os.IsNotExist(err) {
		// create /tmp dir with read and write permission
		if err := os.Mkdir(path, 0755); err != nil {
			return ferror.MakeError(ferror.ErrorInternal, fmt.Sprintf("error while creating directory %s", err.Error()))
		}
		return writeData(data)
	}
	return ferror.MakeError(ferror.ErrorInternal, fmt.Sprintf("error while dumping data %s", err.Error()))
}

// GetByFunction gets a function service from cache using function key.
func (fsc *FunctionServiceCache) GetByFunction(m *metav1.ObjectMeta) (*FuncSvc, error) {
	key := crd.CacheKey(m)

	fsvcI, err := fsc.byFunction.Get(key)
	if err != nil {
		return nil, err
	}

	// update atime
	fsvc := fsvcI.(*FuncSvc)
	fsvc.Atime = time.Now()

	fsvcCopy := *fsvc
	return &fsvcCopy, nil
}

// GetFuncSvc gets a function service from pool cache using function key and returns number of active instances of function pod
func (fsc *FunctionServiceCache) GetFuncSvc(ctx context.Context, m *metav1.ObjectMeta, requestsPerPod int, concurrency int) (*FuncSvc, error) {
	key := crd.CacheKey(m)

	fsvc, err := fsc.connFunctionCache.GetSvcValue(ctx, key, requestsPerPod, concurrency)
	if err != nil {
		fsc.logger.Info("Not found in Cache")
		return nil, err
	}

	// update atime
	fsvc.Atime = time.Now()

	fsvcCopy := *fsvc
	return &fsvcCopy, nil
}

// GetByFunctionUID gets a function service from cache using function UUID.
func (fsc *FunctionServiceCache) GetByFunctionUID(uid types.UID) (*FuncSvc, error) {
	mI, err := fsc.byFunctionUID.Get(uid)
	if err != nil {
		return nil, err
	}

	m := mI.(metav1.ObjectMeta)

	fsvcI, err := fsc.byFunction.Get(crd.CacheKey(&m))
	if err != nil {
		return nil, err
	}

	// update atime
	fsvc := fsvcI.(*FuncSvc)
	fsvc.Atime = time.Now()

	fsvcCopy := *fsvc
	return &fsvcCopy, nil
}

// AddFunc adds a function service to pool cache.
func (fsc *FunctionServiceCache) AddFunc(ctx context.Context, fsvc FuncSvc, requestsPerPod int) {
	fsc.connFunctionCache.SetSvcValue(ctx, crd.CacheKey(fsvc.Function), fsvc.Address, &fsvc, fsvc.CPULimit, requestsPerPod)
	now := time.Now()
	fsvc.Ctime = now
	fsvc.Atime = now
}

// SetCPUUtilizaton updates/sets CPUutilization in the pool cache
func (fsc *FunctionServiceCache) SetCPUUtilizaton(key string, svcHost string, cpuUsage resource.Quantity) {
	fsc.connFunctionCache.SetCPUUtilization(key, svcHost, cpuUsage)
}

// MarkAvailable marks the value at key [function][address] as available.
func (fsc *FunctionServiceCache) MarkAvailable(key string, svcHost string) {
	fsc.connFunctionCache.MarkAvailable(key, svcHost)
}

// Add adds a function service to cache if it does not exist already.
func (fsc *FunctionServiceCache) Add(fsvc FuncSvc) (*FuncSvc, error) {
	existing, err := fsc.byFunction.Set(crd.CacheKey(fsvc.Function), &fsvc)
	if err != nil {
		if IsNameExistError(err) {
			f := existing.(*FuncSvc)
			err2 := fsc.TouchByAddress(f.Address)
			if err2 != nil {
				return nil, err2
			}
			fCopy := *f
			return &fCopy, nil
		}
		return nil, err
	}
	now := time.Now()
	fsvc.Ctime = now
	fsvc.Atime = now

	// Add to byAddress cache. Ignore NameExists errors
	// because of multiple-specialization. See issue #331.
	_, err = fsc.byAddress.Set(fsvc.Address, *fsvc.Function)
	if err != nil {
		if IsNameExistError(err) {
			err = nil
		} else {
			err = errors.Wrap(err, "error caching fsvc")
		}
		return nil, err
	}

	// Add to byFunctionUID cache. Ignore NameExists errors
	// because of multiple-specialization. See issue #331.
	_, err = fsc.byFunctionUID.Set(fsvc.Function.UID, *fsvc.Function)
	if err != nil {
		if IsNameExistError(err) {
			err = nil
		} else {
			err = errors.Wrap(err, "error caching fsvc by function uid")
		}
		return nil, err
	}

	return nil, nil
}

// TouchByAddress makes a TOUCH request to given address.
func (fsc *FunctionServiceCache) TouchByAddress(address string) error {
	responseChannel := make(chan *fscResponse)
	fsc.requestChannel <- &fscRequest{
		requestType:     TOUCH,
		address:         address,
		responseChannel: responseChannel,
	}
	resp := <-responseChannel
	return resp.error
}

func (fsc *FunctionServiceCache) _touchByAddress(address string) error {
	mI, err := fsc.byAddress.Get(address)
	if err != nil {
		return err
	}
	m := mI.(metav1.ObjectMeta)
	fsvcI, err := fsc.byFunction.Get(crd.CacheKey(&m))
	if err != nil {
		return err
	}
	fsvc := fsvcI.(*FuncSvc)
	fsvc.Atime = time.Now()
	return nil
}

// DeleteEntry deletes a function service from cache.
func (fsc *FunctionServiceCache) DeleteEntry(fsvc *FuncSvc) {
	msg := "error deleting function service"
	err := fsc.byFunction.Delete(crd.CacheKey(fsvc.Function))
	if err != nil {
		fsc.logger.Error(
			msg,
			zap.String("function", fsvc.Function.Name),
			zap.Error(err),
		)
	}

	err = fsc.byAddress.Delete(fsvc.Address)
	if err != nil {
		fsc.logger.Error(
			msg,
			zap.String("function", fsvc.Function.Name),
			zap.Error(err),
		)
	}

	err = fsc.byFunctionUID.Delete(fsvc.Function.UID)
	if err != nil {
		fsc.logger.Error(
			msg,
			zap.String("function", fsvc.Function.Name),
			zap.Error(err),
		)
	}

	metrics.FuncRunningSummary.WithLabelValues(fsvc.Function.Name, fsvc.Function.Namespace).Observe(fsvc.Atime.Sub(fsvc.Ctime).Seconds())
}

// DeleteFunctionSvc deletes a function service at key composed of [function][address].
func (fsc *FunctionServiceCache) DeleteFunctionSvc(ctx context.Context, fsvc *FuncSvc) {
	err := fsc.connFunctionCache.DeleteValue(ctx, crd.CacheKey(fsvc.Function), fsvc.Address)
	if err != nil {
		fsc.logger.Error(
			"error deleting function service",
			zap.Any("function", fsvc.Function.Name),
			zap.Any("address", fsvc.Address),
			zap.Error(err),
		)
	}
}

func (fsc *FunctionServiceCache) SetCPUUtilization(key string, svcHost string, cpuUsage resource.Quantity) {
	fsc.connFunctionCache.SetCPUUtilization(key, svcHost, cpuUsage)
}

// DeleteOld deletes aged function service entries from cache.
func (fsc *FunctionServiceCache) DeleteOld(fsvc *FuncSvc, minAge time.Duration) (bool, error) {
	if time.Since(fsvc.Atime) < minAge {
		return false, nil
	}

	fsc.DeleteEntry(fsvc)

	return true, nil
}

// DeleteOldPoolCache deletes aged function service entries from pool cache.
func (fsc *FunctionServiceCache) DeleteOldPoolCache(ctx context.Context, fsvc *FuncSvc, minAge time.Duration) (bool, error) {
	if time.Since(fsvc.Atime) < minAge {
		return false, nil
	}

	fsc.DeleteFunctionSvc(ctx, fsvc)

	return true, nil
}

// ListOld returns a list of aged function services in cache.
func (fsc *FunctionServiceCache) ListOld(age time.Duration) ([]*FuncSvc, error) {
	responseChannel := make(chan *fscResponse)
	fsc.requestChannel <- &fscRequest{
		requestType:     LISTOLD,
		age:             age,
		responseChannel: responseChannel,
	}
	resp := <-responseChannel
	return resp.objects, resp.error
}

// ListOldForPool returns a list of aged function services in cache for pooling.
func (fsc *FunctionServiceCache) ListOldForPool(age time.Duration) ([]*FuncSvc, error) {
	responseChannel := make(chan *fscResponse)
	fsc.requestChannel <- &fscRequest{
		requestType:     LISTOLDPOOL,
		age:             age,
		responseChannel: responseChannel,
	}
	resp := <-responseChannel
	return resp.objects, resp.error
}

// Log makes a LOG type cache request.
func (fsc *FunctionServiceCache) Log() {
	fsc.logger.Info("--- FunctionService Cache Contents")
	responseChannel := make(chan *fscResponse)
	fsc.requestChannel <- &fscRequest{
		requestType:     LOG,
		responseChannel: responseChannel,
	}
	<-responseChannel
	fsc.logger.Info("--- FunctionService Cache Contents End")
}

func GetAttributesForFuncSvc(fsvc *FuncSvc) []attribute.KeyValue {
	if fsvc == nil {
		return []attribute.KeyValue{}
	}
	var attrs []attribute.KeyValue
	if fsvc.Function != nil {
		attrs = append(attrs,
			attribute.KeyValue{Key: "function-name", Value: attribute.StringValue(fsvc.Function.Name)},
			attribute.KeyValue{Key: "function-namespace", Value: attribute.StringValue(fsvc.Function.Namespace)})
	}
	if fsvc.Environment != nil {
		attrs = append(attrs,
			attribute.KeyValue{Key: "environment-name", Value: attribute.StringValue(fsvc.Environment.Name)},
			attribute.KeyValue{Key: "environment-namespace", Value: attribute.StringValue(fsvc.Environment.Namespace)})
	}
	if fsvc.Address != "" {
		attrs = append(attrs, attribute.KeyValue{Key: "address", Value: attribute.StringValue(fsvc.Address)})
	}
	return attrs
}
