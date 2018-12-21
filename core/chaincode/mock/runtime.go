// Code generated by counterfeiter. DO NOT EDIT.
package mock

import (
	"sync"

	"justledger/core/common/ccprovider"
	pb "justledger/protos/peer"
	"golang.org/x/net/context"
)

type Runtime struct {
	StartStub        func(ctxt context.Context, cccid *ccprovider.CCContext, cds *pb.ChaincodeDeploymentSpec) error
	startMutex       sync.RWMutex
	startArgsForCall []struct {
		ctxt  context.Context
		cccid *ccprovider.CCContext
		cds   *pb.ChaincodeDeploymentSpec
	}
	startReturns struct {
		result1 error
	}
	startReturnsOnCall map[int]struct {
		result1 error
	}
	StopStub        func(ctxt context.Context, cccid *ccprovider.CCContext, cds *pb.ChaincodeDeploymentSpec) error
	stopMutex       sync.RWMutex
	stopArgsForCall []struct {
		ctxt  context.Context
		cccid *ccprovider.CCContext
		cds   *pb.ChaincodeDeploymentSpec
	}
	stopReturns struct {
		result1 error
	}
	stopReturnsOnCall map[int]struct {
		result1 error
	}
	invocations      map[string][][]interface{}
	invocationsMutex sync.RWMutex
}

func (fake *Runtime) Start(ctxt context.Context, cccid *ccprovider.CCContext, cds *pb.ChaincodeDeploymentSpec) error {
	fake.startMutex.Lock()
	ret, specificReturn := fake.startReturnsOnCall[len(fake.startArgsForCall)]
	fake.startArgsForCall = append(fake.startArgsForCall, struct {
		ctxt  context.Context
		cccid *ccprovider.CCContext
		cds   *pb.ChaincodeDeploymentSpec
	}{ctxt, cccid, cds})
	fake.recordInvocation("Start", []interface{}{ctxt, cccid, cds})
	fake.startMutex.Unlock()
	if fake.StartStub != nil {
		return fake.StartStub(ctxt, cccid, cds)
	}
	if specificReturn {
		return ret.result1
	}
	return fake.startReturns.result1
}

func (fake *Runtime) StartCallCount() int {
	fake.startMutex.RLock()
	defer fake.startMutex.RUnlock()
	return len(fake.startArgsForCall)
}

func (fake *Runtime) StartArgsForCall(i int) (context.Context, *ccprovider.CCContext, *pb.ChaincodeDeploymentSpec) {
	fake.startMutex.RLock()
	defer fake.startMutex.RUnlock()
	return fake.startArgsForCall[i].ctxt, fake.startArgsForCall[i].cccid, fake.startArgsForCall[i].cds
}

func (fake *Runtime) StartReturns(result1 error) {
	fake.StartStub = nil
	fake.startReturns = struct {
		result1 error
	}{result1}
}

func (fake *Runtime) StartReturnsOnCall(i int, result1 error) {
	fake.StartStub = nil
	if fake.startReturnsOnCall == nil {
		fake.startReturnsOnCall = make(map[int]struct {
			result1 error
		})
	}
	fake.startReturnsOnCall[i] = struct {
		result1 error
	}{result1}
}

func (fake *Runtime) Stop(ctxt context.Context, cccid *ccprovider.CCContext, cds *pb.ChaincodeDeploymentSpec) error {
	fake.stopMutex.Lock()
	ret, specificReturn := fake.stopReturnsOnCall[len(fake.stopArgsForCall)]
	fake.stopArgsForCall = append(fake.stopArgsForCall, struct {
		ctxt  context.Context
		cccid *ccprovider.CCContext
		cds   *pb.ChaincodeDeploymentSpec
	}{ctxt, cccid, cds})
	fake.recordInvocation("Stop", []interface{}{ctxt, cccid, cds})
	fake.stopMutex.Unlock()
	if fake.StopStub != nil {
		return fake.StopStub(ctxt, cccid, cds)
	}
	if specificReturn {
		return ret.result1
	}
	return fake.stopReturns.result1
}

func (fake *Runtime) StopCallCount() int {
	fake.stopMutex.RLock()
	defer fake.stopMutex.RUnlock()
	return len(fake.stopArgsForCall)
}

func (fake *Runtime) StopArgsForCall(i int) (context.Context, *ccprovider.CCContext, *pb.ChaincodeDeploymentSpec) {
	fake.stopMutex.RLock()
	defer fake.stopMutex.RUnlock()
	return fake.stopArgsForCall[i].ctxt, fake.stopArgsForCall[i].cccid, fake.stopArgsForCall[i].cds
}

func (fake *Runtime) StopReturns(result1 error) {
	fake.StopStub = nil
	fake.stopReturns = struct {
		result1 error
	}{result1}
}

func (fake *Runtime) StopReturnsOnCall(i int, result1 error) {
	fake.StopStub = nil
	if fake.stopReturnsOnCall == nil {
		fake.stopReturnsOnCall = make(map[int]struct {
			result1 error
		})
	}
	fake.stopReturnsOnCall[i] = struct {
		result1 error
	}{result1}
}

func (fake *Runtime) Invocations() map[string][][]interface{} {
	fake.invocationsMutex.RLock()
	defer fake.invocationsMutex.RUnlock()
	fake.startMutex.RLock()
	defer fake.startMutex.RUnlock()
	fake.stopMutex.RLock()
	defer fake.stopMutex.RUnlock()
	copiedInvocations := map[string][][]interface{}{}
	for key, value := range fake.invocations {
		copiedInvocations[key] = value
	}
	return copiedInvocations
}

func (fake *Runtime) recordInvocation(key string, args []interface{}) {
	fake.invocationsMutex.Lock()
	defer fake.invocationsMutex.Unlock()
	if fake.invocations == nil {
		fake.invocations = map[string][][]interface{}{}
	}
	if fake.invocations[key] == nil {
		fake.invocations[key] = [][]interface{}{}
	}
	fake.invocations[key] = append(fake.invocations[key], args)
}
