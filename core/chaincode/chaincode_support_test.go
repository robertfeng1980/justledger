/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package chaincode

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang/protobuf/proto"
	"justledger/common/crypto/tlsgen"
	"justledger/common/flogging"
	commonledger "justledger/common/ledger"
	mc "justledger/common/mocks/config"
	mocklgr "justledger/common/mocks/ledger"
	mockpeer "justledger/common/mocks/peer"
	"justledger/common/util"
	"justledger/core/aclmgmt/mocks"
	"justledger/core/aclmgmt/resources"
	"justledger/core/chaincode/accesscontrol"
	"justledger/core/chaincode/mock"
	"justledger/core/chaincode/shim"
	"justledger/core/common/ccprovider"
	"justledger/core/config"
	"justledger/core/container"
	"justledger/core/container/dockercontroller"
	"justledger/core/container/inproccontroller"
	"justledger/core/ledger"
	"justledger/core/ledger/ledgermgmt"
	cmp "justledger/core/mocks/peer"
	"justledger/core/peer"
	"justledger/core/policy"
	"justledger/core/scc"
	mspmgmt "justledger/msp/mgmt"
	plgr "justledger/protos/ledger/queryresult"
	pb "justledger/protos/peer"
	putils "justledger/protos/utils"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/assert"
	"golang.org/x/net/context"
)

var globalBlockNum map[string]uint64

type mockResultsIterator struct {
	current int
	kvs     []*plgr.KV
}

func (mri *mockResultsIterator) Next() (commonledger.QueryResult, error) {
	if mri.current == len(mri.kvs) {
		return nil, nil
	}
	kv := mri.kvs[mri.current]
	mri.current = mri.current + 1

	return kv, nil
}

func (mri *mockResultsIterator) Close() {
	mri.current = len(mri.kvs)
}

type mockExecQuerySimulator struct {
	txsim ledger.TxSimulator
	mocklgr.MockQueryExecutor
	resultsIter map[string]map[string]*mockResultsIterator
}

func (meqe *mockExecQuerySimulator) GetHistoryForKey(namespace, query string) (commonledger.ResultsIterator, error) {
	return meqe.commonQuery(namespace, query)
}

func (meqe *mockExecQuerySimulator) ExecuteQuery(namespace, query string) (commonledger.ResultsIterator, error) {
	return meqe.commonQuery(namespace, query)
}

func (meqe *mockExecQuerySimulator) commonQuery(namespace, query string) (commonledger.ResultsIterator, error) {
	if meqe.resultsIter == nil {
		return nil, fmt.Errorf("query executor not initialized")
	}
	nsiter := meqe.resultsIter[namespace]
	if nsiter == nil {
		return nil, fmt.Errorf("namespace %v not found for %s", namespace, query)
	}
	iter := nsiter[query]
	if iter == nil {
		fmt.Printf("iter not found for query %s\n", query)
	}
	return iter, nil
}

func (meqe *mockExecQuerySimulator) SetState(namespace string, key string, value []byte) error {
	if meqe.txsim == nil {
		return fmt.Errorf("SetState txsimulator not initialed")
	}
	return meqe.txsim.SetState(namespace, key, value)
}

func (meqe *mockExecQuerySimulator) DeleteState(namespace string, key string) error {
	if meqe.txsim == nil {
		return fmt.Errorf("SetState txsimulator not initialed")
	}
	return meqe.txsim.DeleteState(namespace, key)
}

func (meqe *mockExecQuerySimulator) SetStateMultipleKeys(namespace string, kvs map[string][]byte) error {
	if meqe.txsim == nil {
		return fmt.Errorf("SetState txsimulator not initialed")
	}
	return meqe.txsim.SetStateMultipleKeys(namespace, kvs)
}

func (meqe *mockExecQuerySimulator) ExecuteUpdate(query string) error {
	if meqe.txsim == nil {
		return fmt.Errorf("SetState txsimulator not initialed")
	}
	return meqe.txsim.ExecuteUpdate(query)
}

func (meqe *mockExecQuerySimulator) GetTxSimulationResults() ([]byte, error) {
	if meqe.txsim == nil {
		return nil, fmt.Errorf("SetState txsimulator not initialed")
	}
	simRes, err := meqe.txsim.GetTxSimulationResults()
	if err != nil {
		return nil, err
	}
	return simRes.GetPubSimulationBytes()
}

var mockAclProvider *mocks.MockACLProvider

//initialize peer and start up. If security==enabled, login as vp
func initMockPeer(chainIDs ...string) (*ChaincodeSupport, error) {
	msi := &cmp.MockSupportImpl{
		GetApplicationConfigRv:     &mc.MockApplication{CapabilitiesRv: &mc.MockApplicationCapabilities{}},
		GetApplicationConfigBoolRv: true,
	}

	ipRegistry := inproccontroller.NewRegistry()
	sccp := &scc.Provider{Peer: peer.Default, PeerSupport: msi, Registrar: ipRegistry}

	mockAclProvider = &mocks.MockACLProvider{}
	mockAclProvider.Reset()

	peer.MockInitialize()

	mspGetter := func(cid string) []string {
		return []string{"SampleOrg"}
	}

	peer.MockSetMSPIDGetter(mspGetter)

	ccprovider.SetChaincodesPath(ccprovider.GetCCsPath())
	ca, _ := tlsgen.NewCA()
	certGenerator := accesscontrol.NewAuthenticator(ca)
	config := GlobalConfig()
	config.StartupTimeout = 10 * time.Second
	config.ExecuteTimeout = 1 * time.Second
	chaincodeSupport := NewChaincodeSupport(
		config,
		"0.0.0.0:7052",
		true,
		ca.CertBytes(),
		certGenerator,
		&ccprovider.CCInfoFSImpl{},
		mockAclProvider,
		container.NewVMController(
			map[string]container.VMProvider{
				dockercontroller.ContainerType: dockercontroller.NewProvider("", ""),
				inproccontroller.ContainerType: ipRegistry,
			},
		),
		sccp,
	)

	// Mock policy checker
	policy.RegisterPolicyCheckerFactory(&mockPolicyCheckerFactory{})

	ccp := &CCProviderImpl{cs: chaincodeSupport}
	for _, cc := range scc.CreateSysCCs(ccp, sccp, mockAclProvider) {
		sccp.RegisterSysCC(cc)
	}

	globalBlockNum = make(map[string]uint64, len(chainIDs))
	for _, id := range chainIDs {
		if err := peer.MockCreateChain(id); err != nil {
			return nil, err
		}
		sccp.DeploySysCCs(id, ccp)
		// any chain other than the default testchainid does not have a MSP set up -> create one
		if id != util.GetTestChainID() {
			mspmgmt.XXXSetMSPManager(id, mspmgmt.GetManagerForChain(util.GetTestChainID()))
		}
		globalBlockNum[id] = 1
	}

	return chaincodeSupport, nil
}

func finitMockPeer(chainIDs ...string) {
	for _, c := range chainIDs {
		if lgr := peer.GetLedger(c); lgr != nil {
			lgr.Close()
		}
	}
	ledgermgmt.CleanupTestEnv()
	ledgerPath := config.GetPath("peer.fileSystemPath")
	os.RemoveAll(ledgerPath)
	os.RemoveAll(filepath.Join(os.TempDir(), "hyperledger"))
}

//store the stream CC mappings here
var mockPeerCCSupport = mockpeer.NewMockPeerSupport()

func mockChaincodeStreamGetter(name string) (shim.PeerChaincodeStream, error) {
	return mockPeerCCSupport.GetCC(name)
}

type mockCCLauncher struct {
	execTime *time.Duration
	resp     error
	retErr   error
	notfyb   bool
}

func (ccl *mockCCLauncher) launch(ctxt context.Context, notfy chan bool) error {
	if ccl.execTime != nil {
		time.Sleep(*ccl.execTime)
	}

	//no error on launch, notify
	if ccl.resp == nil {
		notfy <- ccl.notfyb
	}

	return ccl.retErr
}

func setupcc(name string) (*mockpeer.MockCCComm, *mockpeer.MockCCComm) {
	send := make(chan *pb.ChaincodeMessage)
	recv := make(chan *pb.ChaincodeMessage)
	peerSide, _ := mockPeerCCSupport.AddCC(name, recv, send)
	peerSide.SetName("peer")
	ccSide := mockPeerCCSupport.GetCCMirror(name)
	ccSide.SetPong(true)
	return peerSide, ccSide
}

//assign this to done and failNow and keep using them
func setuperror() chan error {
	return make(chan error)
}

func processDone(t *testing.T, done chan error, expecterr bool) {
	var err error
	if done != nil {
		err = <-done
	}
	if expecterr != (err != nil) {
		if err == nil {
			t.Fatalf("Expected error but got success")
		} else {
			t.Fatalf("Expected success but got error %s", err)
		}
	}
}

func startTx(t *testing.T, chainID string, cis *pb.ChaincodeInvocationSpec, txId string) (context.Context, ledger.TxSimulator, *pb.SignedProposal, *pb.Proposal) {
	ctxt := context.Background()

	creator := []byte([]byte("Alice"))
	sprop, prop := putils.MockSignedEndorserProposalOrPanic(chainID, cis.ChaincodeSpec, creator, []byte("msg1"))
	var txsim ledger.TxSimulator
	var err error
	if ctxt, txsim, err = startTxSimulation(ctxt, chainID, txId); err != nil {
		t.Fatalf("getting txsimulator failed %s", err)
	}
	return ctxt, txsim, sprop, prop
}

func endTx(t *testing.T, cccid *ccprovider.CCContext, txsim ledger.TxSimulator, cis *pb.ChaincodeInvocationSpec) {
	if err := endTxSimulationCIS(cccid.ChainID, cis.ChaincodeSpec.ChaincodeId, cccid.TxID, txsim, []byte("invoke"), true, cis, globalBlockNum[cccid.ChainID]); err != nil {
		t.Fatalf("simulation failed with error %s", err)
	}
	globalBlockNum[cccid.ChainID] = globalBlockNum[cccid.ChainID] + 1
}

func execCC(t *testing.T, ctxt context.Context, ccSide *mockpeer.MockCCComm, cccid *ccprovider.CCContext, waitForERROR bool, expectExecErr bool, done chan error, cis *pb.ChaincodeInvocationSpec, respSet *mockpeer.MockResponseSet, chaincodeSupport *ChaincodeSupport) error {
	ccSide.SetResponses(respSet)

	resp, _, err := chaincodeSupport.Execute(ctxt, cccid, cis)
	if err == nil && resp.Status != shim.OK {
		err = errors.New(resp.Message)
	}

	if err == nil && expectExecErr {
		t.Fatalf("expected error but succeeded")
	} else if err != nil && !expectExecErr {
		t.Fatalf("exec failed with %s", err)
	}

	//wait
	processDone(t, done, waitForERROR)

	return nil
}

//initialize cc support env and startup the chaincode
func startCC(t *testing.T, channelID string, ccname string, chaincodeSupport *ChaincodeSupport) (*mockpeer.MockCCComm, *mockpeer.MockCCComm) {
	peerSide, ccSide := setupcc(ccname)
	defer mockPeerCCSupport.RemoveCC(ccname)
	flogging.SetModuleLevel("chaincode", "debug")
	//register peer side with ccsupport
	go func() {
		chaincodeSupport.HandleChaincodeStream(context.Background(), peerSide)
	}()

	done := setuperror()

	errorFunc := func(ind int, err error) {
		done <- err
	}

	//start the mock peer
	go func() {
		respSet := &mockpeer.MockResponseSet{
			DoneFunc:  errorFunc,
			ErrorFunc: nil,
			Responses: []*mockpeer.MockResponse{
				{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_REGISTERED}, nil},
				{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_READY}, nil},
			},
		}
		ccSide.SetResponses(respSet)
		ccSide.Run()
	}()

	ccSide.Send(&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_REGISTER, Payload: putils.MarshalOrPanic(&pb.ChaincodeID{Name: ccname + ":0"}), Txid: "0", ChannelId: channelID})

	//wait for init
	processDone(t, done, false)

	return peerSide, ccSide
}

func getTarGZ(t *testing.T, name string, contents []byte) []byte {
	startTime := time.Now()
	inputbuf := bytes.NewBuffer(nil)
	gw := gzip.NewWriter(inputbuf)
	tr := tar.NewWriter(gw)
	size := int64(len(contents))

	tr.WriteHeader(&tar.Header{Name: name, Size: size, ModTime: startTime, AccessTime: startTime, ChangeTime: startTime})
	tr.Write(contents)
	tr.Close()
	gw.Close()
	ioutil.WriteFile("/tmp/t.gz", inputbuf.Bytes(), 0644)
	return inputbuf.Bytes()
}

// Deploy a chaincode - i.e., build and initialize.
func deployCC(t *testing.T, ctx context.Context, cccid *ccprovider.CCContext, spec *pb.ChaincodeSpec, chaincodeSupport *ChaincodeSupport) {
	// First build and get the deployment spec
	code := getTarGZ(t, "src/dummy/dummy.go", []byte("code"))
	cds := &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec, CodePackage: code}

	//ignore existence errors
	ccprovider.PutChaincodeIntoFS(cds)

	b := putils.MarshalOrPanic(cds)

	sysCCVers := util.GetSysCCVersion()

	//wrap the deployment in an invocation spec to lscc...
	lsccSpec := &pb.ChaincodeInvocationSpec{ChaincodeSpec: &pb.ChaincodeSpec{Type: pb.ChaincodeSpec_GOLANG, ChaincodeId: &pb.ChaincodeID{Name: "lscc", Version: sysCCVers}, Input: &pb.ChaincodeInput{Args: [][]byte{[]byte("deploy"), []byte(cccid.ChainID), b}}}}

	sprop, prop := putils.MockSignedEndorserProposal2OrPanic(cccid.ChainID, lsccSpec.ChaincodeSpec, signer)
	lsccid := ccprovider.NewCCContext(cccid.ChainID, lsccSpec.ChaincodeSpec.ChaincodeId.Name, sysCCVers, cccid.TxID, true, sprop, prop)

	//write to lscc
	if _, _, err := chaincodeSupport.Execute(ctx, lsccid, lsccSpec); err != nil {
		t.Fatalf("Error deploying chaincode %v (err: %s)", cccid, err)
	}
}

func initializeCC(t *testing.T, chainID, ccname string, ccSide *mockpeer.MockCCComm, chaincodeSupport *ChaincodeSupport) error {
	done := setuperror()

	errorFunc := func(ind int, err error) {
		done <- err
	}

	chaincodeID := &pb.ChaincodeID{Name: ccname, Version: "0"}
	ci := &pb.ChaincodeInput{Args: [][]byte{[]byte("init"), []byte("A"), []byte("100"), []byte("B"), []byte("200")}, Decorations: nil}
	cis := &pb.ChaincodeInvocationSpec{ChaincodeSpec: &pb.ChaincodeSpec{Type: pb.ChaincodeSpec_Type(pb.ChaincodeSpec_Type_value["GOLANG"]), ChaincodeId: chaincodeID, Input: ci}}

	txid := util.GenerateUUID()
	ctxt, txsim, sprop, prop := startTx(t, chainID, cis, txid)

	//setup CheckACL calls
	mockAclProvider.Reset()
	mockAclProvider.On("CheckACL", resources.Lscc_GetDeploymentSpec, chainID, sprop).Return(nil)
	mockAclProvider.On("CheckACL", resources.Lscc_GetChaincodeData, chainID, sprop).Return(nil)
	mockAclProvider.On("CheckACL", resources.Peer_Propose, chainID, sprop).Return(nil)

	//bad txid in response (should be "1"), should fail
	resp := &mockpeer.MockResponse{
		RecvMsg: &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_TRANSACTION},
		RespMsg: &pb.ChaincodeMessage{
			Type:      pb.ChaincodeMessage_COMPLETED,
			Payload:   putils.MarshalOrPanic(&pb.Response{Status: shim.OK, Payload: []byte("init succeeded")}),
			Txid:      "unknowntxid",
			ChannelId: chainID,
		},
	}
	respSet := &mockpeer.MockResponseSet{
		DoneFunc:  errorFunc,
		ErrorFunc: nil,
		Responses: []*mockpeer.MockResponse{resp},
	}

	cccid := ccprovider.NewCCContext(chainID, ccname, "0", txid, false, sprop, prop)
	execCC(t, ctxt, ccSide, cccid, false, true, done, cis, respSet, chaincodeSupport)

	//set the right TxID in response now
	resp.RespMsg.(*pb.ChaincodeMessage).Txid = txid

	badcccid := ccprovider.NewCCContext(chainID, ccname, "unknownver", txid, false, sprop, prop)

	//we are not going to reach the chaincode and so won't get a response from it. processDone will not
	//be triggered by the chaincode stream.  We just expect an error from fabric. Hence pass nil for done
	execCC(t, ctxt, ccSide, badcccid, false, true, nil, cis, respSet, chaincodeSupport)

	//---------try a successful init at last-------
	//everything lined up
	//    correct registered chaincode version
	//    matching txid
	//    txsim context
	//    full response
	//    correct block number for ending sim

	respSet = &mockpeer.MockResponseSet{errorFunc, nil, []*mockpeer.MockResponse{
		{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_TRANSACTION}, &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_PUT_STATE, Payload: putils.MarshalOrPanic(&pb.PutState{Collection: "", Key: "A", Value: []byte("100")}), Txid: txid, ChannelId: chainID}},
		{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_RESPONSE}, &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_PUT_STATE, Payload: putils.MarshalOrPanic(&pb.PutState{Collection: "", Key: "B", Value: []byte("200")}), Txid: txid, ChannelId: chainID}},
		// The following private data parameters are disabled because
		// this requires private data channel capability ON and hence should be present
		// in a dedicated test. One such test is present in file - executetransaction_pvtdata_test.go
		// {&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_RESPONSE}, &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_PUT_STATE, Payload: putils.MarshalOrPanic(&pb.PutState{Collection: "c1", Key: "C", Value: []byte("300")}), Txid: txid, ChannelId: chainID}},
		// {&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_RESPONSE}, &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_PUT_STATE, Payload: putils.MarshalOrPanic(&pb.PutState{Collection: "c2", Key: "C", Value: []byte("300")}), Txid: txid, ChannelId: chainID}},
		{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_RESPONSE}, &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_COMPLETED, Payload: putils.MarshalOrPanic(&pb.Response{Status: shim.OK, Payload: []byte("OK")}), ChaincodeEvent: &pb.ChaincodeEvent{ChaincodeId: ccname}, Txid: txid, ChannelId: chainID}}}}

	cccid.Version = "1"
	execCC(t, ctxt, ccSide, cccid, false, false, done, cis, respSet, chaincodeSupport)

	endTx(t, cccid, txsim, cis)

	return nil
}

func invokeCC(t *testing.T, chainID, ccname string, ccSide *mockpeer.MockCCComm, chaincodeSupport *ChaincodeSupport) error {
	done := setuperror()

	errorFunc := func(ind int, err error) {
		done <- err
	}

	chaincodeID := &pb.ChaincodeID{Name: ccname, Version: "0"}
	ci := &pb.ChaincodeInput{Args: [][]byte{[]byte("invoke"), []byte("A"), []byte("B"), []byte("10")}, Decorations: nil}
	cis := &pb.ChaincodeInvocationSpec{ChaincodeSpec: &pb.ChaincodeSpec{Type: pb.ChaincodeSpec_Type(pb.ChaincodeSpec_Type_value["GOLANG"]), ChaincodeId: chaincodeID, Input: ci}}
	txid := util.GenerateUUID()
	ctxt, txsim, sprop, prop := startTx(t, chainID, cis, txid)

	//setup CheckACL calls
	mockAclProvider.Reset()
	mockAclProvider.On("CheckACL", resources.Lscc_GetDeploymentSpec, chainID, sprop).Return(nil)
	mockAclProvider.On("CheckACL", resources.Lscc_GetChaincodeData, chainID, sprop).Return(nil)
	mockAclProvider.On("CheckACL", resources.Peer_Propose, chainID, sprop).Return(nil)

	respSet := &mockpeer.MockResponseSet{
		DoneFunc:  errorFunc,
		ErrorFunc: nil,
		Responses: []*mockpeer.MockResponse{
			{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_TRANSACTION}, &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_GET_STATE, Payload: putils.MarshalOrPanic(&pb.GetState{Collection: "", Key: "A"}), Txid: txid, ChannelId: chainID}},
			{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_RESPONSE}, &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_GET_STATE, Payload: putils.MarshalOrPanic(&pb.GetState{Collection: "", Key: "B"}), Txid: txid, ChannelId: chainID}},
			{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_TRANSACTION}, &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_PUT_STATE, Payload: putils.MarshalOrPanic(&pb.PutState{Collection: "", Key: "A", Value: []byte("90")}), Txid: txid, ChannelId: chainID}},
			{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_RESPONSE}, &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_PUT_STATE, Payload: putils.MarshalOrPanic(&pb.PutState{Collection: "", Key: "B", Value: []byte("210")}), Txid: txid, ChannelId: chainID}},
			{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_RESPONSE}, &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_PUT_STATE, Payload: putils.MarshalOrPanic(&pb.PutState{Collection: "", Key: "TODEL", Value: []byte("-to-be-deleted-")}), Txid: txid, ChannelId: chainID}},
			{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_RESPONSE}, &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_COMPLETED, Payload: putils.MarshalOrPanic(&pb.Response{Status: shim.OK, Payload: []byte("OK")}), Txid: txid, ChannelId: chainID}},
		},
	}

	cccid := ccprovider.NewCCContext(chainID, ccname, "0", txid, false, sprop, prop)
	execCC(t, ctxt, ccSide, cccid, false, false, done, cis, respSet, chaincodeSupport)

	//delete the extra var
	respSet = &mockpeer.MockResponseSet{
		DoneFunc:  errorFunc,
		ErrorFunc: nil,
		Responses: []*mockpeer.MockResponse{
			{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_TRANSACTION}, &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_GET_STATE, Payload: putils.MarshalOrPanic(&pb.GetState{Collection: "", Key: "TODEL"}), Txid: "3", ChannelId: chainID}},
			{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_RESPONSE}, &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_DEL_STATE, Payload: putils.MarshalOrPanic(&pb.DelState{Collection: "", Key: "TODEL"}), Txid: "3", ChannelId: chainID}},
			{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_RESPONSE}, &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_COMPLETED, Payload: putils.MarshalOrPanic(&pb.Response{Status: shim.OK, Payload: []byte("OK")}), Txid: "3", ChannelId: chainID}},
		},
	}

	cccid.TxID = "3"
	execCC(t, ctxt, ccSide, cccid, false, false, done, cis, respSet, chaincodeSupport)

	//get the extra var and delete it
	respSet = &mockpeer.MockResponseSet{
		DoneFunc:  errorFunc,
		ErrorFunc: nil,
		Responses: []*mockpeer.MockResponse{
			{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_TRANSACTION}, &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_GET_STATE, Payload: putils.MarshalOrPanic(&pb.GetState{Collection: "", Key: "TODEL"}), Txid: "4", ChannelId: chainID}},
			{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_ERROR}, &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_COMPLETED, Payload: putils.MarshalOrPanic(&pb.Response{Status: shim.ERROR, Message: "variable not found"}), Txid: "4", ChannelId: chainID}},
		},
	}

	cccid.TxID = "4"
	execCC(t, ctxt, ccSide, cccid, false, true, done, cis, respSet, chaincodeSupport)

	endTx(t, cccid, txsim, cis)

	return nil
}

func invokePrivateDataGetPutDelCC(t *testing.T, chainID, ccname string, ccSide *mockpeer.MockCCComm, chaincodeSupport *ChaincodeSupport) error {
	done := setuperror()

	errorFunc := func(ind int, err error) {
		done <- err
	}

	chaincodeID := &pb.ChaincodeID{Name: ccname, Version: "0"}
	ci := &pb.ChaincodeInput{Args: [][]byte{[]byte("invokePrivateData")}, Decorations: nil}
	cis := &pb.ChaincodeInvocationSpec{ChaincodeSpec: &pb.ChaincodeSpec{Type: pb.ChaincodeSpec_Type(pb.ChaincodeSpec_Type_value["GOLANG"]), ChaincodeId: chaincodeID, Input: ci}}
	txid := util.GenerateUUID()
	ctxt, txsim, sprop, prop := startTx(t, chainID, cis, txid)

	respSet := &mockpeer.MockResponseSet{
		DoneFunc:  errorFunc,
		ErrorFunc: nil,
		Responses: []*mockpeer.MockResponse{
			{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_TRANSACTION}, &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_GET_STATE, Payload: putils.MarshalOrPanic(&pb.GetState{Collection: "", Key: "C"}), Txid: txid, ChannelId: chainID}},
			{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_ERROR}, &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_COMPLETED, Payload: putils.MarshalOrPanic(&pb.Response{Status: shim.ERROR, Message: "variable not found"}), Txid: txid, ChannelId: chainID}},
		},
	}

	cccid := ccprovider.NewCCContext(chainID, ccname, "0", txid, false, sprop, prop)
	execCC(t, ctxt, ccSide, cccid, false, true, done, cis, respSet, chaincodeSupport)

	respSet = &mockpeer.MockResponseSet{
		DoneFunc:  errorFunc,
		ErrorFunc: nil,
		Responses: []*mockpeer.MockResponse{
			{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_RESPONSE}, &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_GET_STATE, Payload: putils.MarshalOrPanic(&pb.GetState{Collection: "c1", Key: "C"}), Txid: txid, ChannelId: chainID}},
			{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_RESPONSE}, &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_PUT_STATE, Payload: putils.MarshalOrPanic(&pb.PutState{Collection: "c1", Key: "C", Value: []byte("310")}), Txid: txid, ChannelId: chainID}},
			{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_RESPONSE}, &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_PUT_STATE, Payload: putils.MarshalOrPanic(&pb.PutState{Collection: "c1", Key: "A", Value: []byte("100")}), Txid: txid, ChannelId: chainID}},
			{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_RESPONSE}, &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_PUT_STATE, Payload: putils.MarshalOrPanic(&pb.PutState{Collection: "c1", Key: "B", Value: []byte("100")}), Txid: txid, ChannelId: chainID}},
			{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_RESPONSE}, &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_DEL_STATE, Payload: putils.MarshalOrPanic(&pb.DelState{Collection: "c2", Key: "C"}), Txid: txid, ChannelId: chainID}},
			{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_RESPONSE}, &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_COMPLETED, Payload: putils.MarshalOrPanic(&pb.Response{Status: shim.OK, Payload: []byte("OK")}), Txid: txid, ChannelId: chainID}},
		},
	}

	cccid = ccprovider.NewCCContext(chainID, ccname, "0", txid, false, sprop, prop)
	execCC(t, ctxt, ccSide, cccid, false, false, done, cis, respSet, chaincodeSupport)

	endTx(t, cccid, txsim, cis)

	txid = util.GenerateUUID()
	ctxt, txsim, sprop, prop = startTx(t, chainID, cis, txid)

	respSet = &mockpeer.MockResponseSet{
		DoneFunc:  errorFunc,
		ErrorFunc: nil,
		Responses: []*mockpeer.MockResponse{
			{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_TRANSACTION}, &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_GET_STATE, Payload: putils.MarshalOrPanic(&pb.GetState{Collection: "c2", Key: "C"}), Txid: txid, ChannelId: chainID}},
			{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_ERROR}, &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_COMPLETED, Payload: putils.MarshalOrPanic(&pb.Response{Status: shim.ERROR, Message: "variable not found"}), Txid: txid, ChannelId: chainID}},
		},
	}

	cccid = ccprovider.NewCCContext(chainID, ccname, "0", txid, false, sprop, prop)
	execCC(t, ctxt, ccSide, cccid, false, true, done, cis, respSet, chaincodeSupport)

	endTx(t, cccid, txsim, cis)

	return nil
}

func getQueryStateByRange(t *testing.T, collection, chainID, ccname string, ccSide *mockpeer.MockCCComm, chaincodeSupport *ChaincodeSupport) error {
	done := setuperror()

	errorFunc := func(ind int, err error) {
		done <- err
	}

	chaincodeID := &pb.ChaincodeID{Name: ccname, Version: "0"}
	ci := &pb.ChaincodeInput{Args: [][]byte{[]byte("invoke"), []byte("A"), []byte("B"), []byte("10")}, Decorations: nil}
	cis := &pb.ChaincodeInvocationSpec{ChaincodeSpec: &pb.ChaincodeSpec{Type: pb.ChaincodeSpec_Type(pb.ChaincodeSpec_Type_value["GOLANG"]), ChaincodeId: chaincodeID, Input: ci}}
	txid := util.GenerateUUID()
	ctxt, txsim, sprop, prop := startTx(t, chainID, cis, txid)

	//setup CheckACL calls
	mockAclProvider.Reset()
	mockAclProvider.On("CheckACL", resources.Lscc_GetDeploymentSpec, chainID, sprop).Return(nil)
	mockAclProvider.On("CheckACL", resources.Lscc_GetChaincodeData, chainID, sprop).Return(nil)
	mockAclProvider.On("CheckACL", resources.Peer_Propose, chainID, sprop).Return(nil)

	//create the response
	queryStateNextFunc := func(reqMsg *pb.ChaincodeMessage) *pb.ChaincodeMessage {
		qr := &pb.QueryResponse{}
		proto.Unmarshal(reqMsg.Payload, qr)
		return &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_QUERY_STATE_NEXT, Payload: putils.MarshalOrPanic(&pb.QueryStateNext{Id: qr.Id}), Txid: txid, ChannelId: chainID}
	}
	queryStateCloseFunc := func(reqMsg *pb.ChaincodeMessage) *pb.ChaincodeMessage {
		qr := &pb.QueryResponse{}
		proto.Unmarshal(reqMsg.Payload, qr)
		return &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_QUERY_STATE_CLOSE, Payload: putils.MarshalOrPanic(&pb.QueryStateClose{Id: qr.Id}), Txid: txid, ChannelId: chainID}
	}

	var mkpeer []*mockpeer.MockResponse

	mkpeer = append(mkpeer, &mockpeer.MockResponse{
		RecvMsg: &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_TRANSACTION},
		RespMsg: &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_GET_STATE_BY_RANGE, Payload: putils.MarshalOrPanic(&pb.GetStateByRange{Collection: collection, StartKey: "A", EndKey: "B"}), Txid: txid, ChannelId: chainID},
	})

	if collection == "" {
		mkpeer = append(mkpeer, &mockpeer.MockResponse{
			RecvMsg: &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_RESPONSE},
			RespMsg: queryStateNextFunc,
		})
		mkpeer = append(mkpeer, &mockpeer.MockResponse{
			RecvMsg: &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_ERROR},
			RespMsg: queryStateCloseFunc,
		})
		mkpeer = append(mkpeer, &mockpeer.MockResponse{
			RecvMsg: &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_RESPONSE},
			RespMsg: &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_COMPLETED, Payload: putils.MarshalOrPanic(&pb.Response{Status: shim.OK, Payload: []byte("OK")}), Txid: txid, ChannelId: chainID},
		})
	} else {
		// Range queries on private data is not yet implemented.
		mkpeer = append(mkpeer, &mockpeer.MockResponse{
			RecvMsg: &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_ERROR},
			RespMsg: &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_COMPLETED, Payload: putils.MarshalOrPanic(&pb.Response{Status: shim.ERROR, Message: "Not Yet Supported"}), Txid: txid, ChannelId: chainID},
		})
	}

	respSet := &mockpeer.MockResponseSet{
		DoneFunc:  errorFunc,
		ErrorFunc: nil,
		Responses: mkpeer,
	}

	cccid := ccprovider.NewCCContext(chainID, ccname, "0", txid, false, sprop, prop)
	if collection == "" {
		execCC(t, ctxt, ccSide, cccid, false, false, done, cis, respSet, chaincodeSupport)
	} else {
		execCC(t, ctxt, ccSide, cccid, false, true, done, cis, respSet, chaincodeSupport)
	}

	endTx(t, cccid, txsim, cis)

	return nil
}

func cc2cc(t *testing.T, chainID, chainID2, ccname string, ccSide *mockpeer.MockCCComm, chaincodeSupport *ChaincodeSupport) error {
	calledCC := "calledCC"
	//starts and registers the CC
	_, calledCCSide := startCC(t, chainID2, calledCC, chaincodeSupport)
	if calledCCSide == nil {
		t.Fatalf("start up failed for called CC")
	}
	defer calledCCSide.Quit()

	done := setuperror()

	errorFunc := func(ind int, err error) {
		done <- err
	}

	chaincodeID := &pb.ChaincodeID{Name: calledCC, Version: "0"}
	ci := &pb.ChaincodeInput{Args: [][]byte{[]byte("deploycc")}, Decorations: nil}
	cis := &pb.ChaincodeInvocationSpec{ChaincodeSpec: &pb.ChaincodeSpec{Type: pb.ChaincodeSpec_Type(pb.ChaincodeSpec_Type_value["GOLANG"]), ChaincodeId: chaincodeID, Input: ci}}
	txid := util.GenerateUUID()
	//first deploy the new cc to LSCC
	ctxt, txsim, sprop, prop := startTx(t, chainID, cis, txid)

	//setup CheckACL calls
	mockAclProvider.Reset()
	mockAclProvider.On("CheckACL", resources.Peer_ChaincodeToChaincode, chainID, sprop).Return(nil)
	mockAclProvider.On("CheckACL", resources.Lscc_GetDeploymentSpec, chainID, sprop).Return(nil)
	mockAclProvider.On("CheckACL", resources.Lscc_GetChaincodeData, chainID, sprop).Return(nil)
	mockAclProvider.On("CheckACL", resources.Peer_Propose, chainID, sprop).Return(nil)

	cccid := ccprovider.NewCCContext(chainID, calledCC, "0", txid, false, sprop, prop)

	deployCC(t, ctxt, cccid, cis.ChaincodeSpec, chaincodeSupport)

	//commit
	endTx(t, cccid, txsim, cis)

	//now do the cc2cc
	chaincodeID = &pb.ChaincodeID{Name: ccname, Version: "0"}
	ci = &pb.ChaincodeInput{Args: [][]byte{[]byte("invokecc")}, Decorations: nil}
	cis = &pb.ChaincodeInvocationSpec{ChaincodeSpec: &pb.ChaincodeSpec{Type: pb.ChaincodeSpec_Type(pb.ChaincodeSpec_Type_value["GOLANG"]), ChaincodeId: chaincodeID, Input: ci}}
	txid = util.GenerateUUID()
	ctxt, txsim, sprop, prop = startTx(t, chainID, cis, txid)

	//we want hooks for both chaincodes
	mockAclProvider.Reset()
	mockAclProvider.On("CheckACL", resources.Peer_ChaincodeToChaincode, chainID2, sprop).Return(nil)
	mockAclProvider.On("CheckACL", resources.Peer_ChaincodeToChaincode, chainID, sprop).Return(nil)
	mockAclProvider.On("CheckACL", resources.Lscc_GetDeploymentSpec, chainID2, sprop).Return(nil)
	mockAclProvider.On("CheckACL", resources.Lscc_GetDeploymentSpec, chainID, sprop).Return(nil)
	mockAclProvider.On("CheckACL", resources.Lscc_GetChaincodeData, chainID2, sprop).Return(nil)
	mockAclProvider.On("CheckACL", resources.Lscc_GetChaincodeData, chainID, sprop).Return(nil)
	mockAclProvider.On("CheckACL", resources.Peer_Propose, chainID, sprop).Return(nil)

	sysCCVers := util.GetSysCCVersion()
	//call a callable system CC, a regular cc, a regular but different cc on a different chain, a regular but same cc on a different chain,  and an uncallable system cc and expect an error inthe last one
	respSet := &mockpeer.MockResponseSet{
		DoneFunc:  errorFunc,
		ErrorFunc: nil,
		Responses: []*mockpeer.MockResponse{
			{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_TRANSACTION}, &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_INVOKE_CHAINCODE, Payload: putils.MarshalOrPanic(&pb.ChaincodeSpec{ChaincodeId: &pb.ChaincodeID{Name: "lscc:" + sysCCVers}, Input: &pb.ChaincodeInput{Args: [][]byte{{}}}}), Txid: txid, ChannelId: chainID}},
			{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_RESPONSE}, &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_INVOKE_CHAINCODE, Payload: putils.MarshalOrPanic(&pb.ChaincodeSpec{ChaincodeId: &pb.ChaincodeID{Name: "calledCC:0/" + chainID}, Input: &pb.ChaincodeInput{Args: [][]byte{{}}}}), Txid: txid, ChannelId: chainID}},
			{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_RESPONSE}, &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_INVOKE_CHAINCODE, Payload: putils.MarshalOrPanic(&pb.ChaincodeSpec{ChaincodeId: &pb.ChaincodeID{Name: "calledCC:0/" + chainID2}, Input: &pb.ChaincodeInput{Args: [][]byte{{}}}}), Txid: txid, ChannelId: chainID}},
			{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_RESPONSE}, &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_INVOKE_CHAINCODE, Payload: putils.MarshalOrPanic(&pb.ChaincodeSpec{ChaincodeId: &pb.ChaincodeID{Name: "vscc:" + sysCCVers}, Input: &pb.ChaincodeInput{Args: [][]byte{{}}}}), Txid: txid, ChannelId: chainID}},
		},
	}

	respSet2 := &mockpeer.MockResponseSet{
		DoneFunc:  nil,
		ErrorFunc: nil,
		Responses: []*mockpeer.MockResponse{
			{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_TRANSACTION}, &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_COMPLETED, Payload: putils.MarshalOrPanic(&pb.Response{Status: shim.OK, Payload: []byte("OK")}), Txid: txid, ChannelId: chainID}},
		},
	}
	calledCCSide.SetResponses(respSet2)

	cccid = ccprovider.NewCCContext(chainID, ccname, "0", txid, false, sprop, prop)

	execCC(t, ctxt, ccSide, cccid, false, true, done, cis, respSet, chaincodeSupport)

	endTx(t, cccid, txsim, cis)

	//finally lets try a Bad ACL with CC-calling-CC
	chaincodeID = &pb.ChaincodeID{Name: ccname, Version: "0"}
	ci = &pb.ChaincodeInput{Args: [][]byte{[]byte("invokecc")}, Decorations: nil}
	cis = &pb.ChaincodeInvocationSpec{ChaincodeSpec: &pb.ChaincodeSpec{Type: pb.ChaincodeSpec_Type(pb.ChaincodeSpec_Type_value["GOLANG"]), ChaincodeId: chaincodeID, Input: ci}}
	txid = util.GenerateUUID()
	ctxt, txsim, sprop, prop = startTx(t, chainID, cis, txid)

	mockAclProvider.Reset()
	mockAclProvider.On("CheckACL", resources.Peer_ChaincodeToChaincode, chainID2, sprop).Return(nil)
	mockAclProvider.On("CheckACL", resources.Peer_ChaincodeToChaincode, chainID, sprop).Return(errors.New("Bad ACL calling CC"))
	mockAclProvider.On("CheckACL", resources.Lscc_GetDeploymentSpec, chainID2, sprop).Return(nil)
	mockAclProvider.On("CheckACL", resources.Lscc_GetDeploymentSpec, chainID, sprop).Return(nil)
	mockAclProvider.On("CheckACL", resources.Lscc_GetChaincodeData, chainID2, sprop).Return(nil)
	mockAclProvider.On("CheckACL", resources.Lscc_GetChaincodeData, chainID, sprop).Return(nil)
	mockAclProvider.On("CheckACL", resources.Peer_Propose, chainID, sprop).Return(nil)
	//call regular cc but without ACL on called CC
	respSet = &mockpeer.MockResponseSet{
		DoneFunc:  errorFunc,
		ErrorFunc: nil,
		Responses: []*mockpeer.MockResponse{
			{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_RESPONSE}, &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_INVOKE_CHAINCODE, Payload: putils.MarshalOrPanic(&pb.ChaincodeSpec{ChaincodeId: &pb.ChaincodeID{Name: "calledCC:0/" + chainID}, Input: &pb.ChaincodeInput{Args: [][]byte{{}}}}), Txid: txid, ChannelId: chainID}},
		},
	}

	respSet2 = &mockpeer.MockResponseSet{
		DoneFunc:  nil,
		ErrorFunc: nil,
		Responses: []*mockpeer.MockResponse{
			{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_TRANSACTION}, &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_COMPLETED, Payload: putils.MarshalOrPanic(&pb.Response{Status: shim.OK, Payload: []byte("OK")}), Txid: txid, ChannelId: chainID}},
		},
	}

	calledCCSide.SetResponses(respSet2)

	cccid = ccprovider.NewCCContext(chainID, ccname, "0", txid, false, sprop, prop)

	execCC(t, ctxt, ccSide, cccid, false, true, done, cis, respSet, chaincodeSupport)

	endTx(t, cccid, txsim, cis)

	return nil
}

func getQueryResult(t *testing.T, collection, chainID, ccname string, ccSide *mockpeer.MockCCComm, chaincodeSupport *ChaincodeSupport) error {
	done := setuperror()

	errorFunc := func(ind int, err error) {
		done <- err
	}

	chaincodeID := &pb.ChaincodeID{Name: ccname, Version: "0"}
	ci := &pb.ChaincodeInput{Args: [][]byte{[]byte("invoke"), []byte("A"), []byte("B"), []byte("10")}, Decorations: nil}
	cis := &pb.ChaincodeInvocationSpec{ChaincodeSpec: &pb.ChaincodeSpec{Type: pb.ChaincodeSpec_Type(pb.ChaincodeSpec_Type_value["GOLANG"]), ChaincodeId: chaincodeID, Input: ci}}
	txid := util.GenerateUUID()
	ctxt, txsim, sprop, prop := startTx(t, chainID, cis, txid)

	mockAclProvider.Reset()
	mockAclProvider.On("CheckACL", resources.Lscc_GetDeploymentSpec, chainID, sprop).Return(nil)
	mockAclProvider.On("CheckACL", resources.Lscc_GetChaincodeData, chainID, sprop).Return(nil)
	mockAclProvider.On("CheckACL", resources.Peer_Propose, chainID, sprop).Return(nil)

	kvs := make([]*plgr.KV, 1000)
	for i := 0; i < 1000; i++ {
		kvs[i] = &plgr.KV{Namespace: chainID, Key: fmt.Sprintf("%d", i), Value: []byte(fmt.Sprintf("%d", i))}
	}

	queryExec := &mockExecQuerySimulator{resultsIter: make(map[string]map[string]*mockResultsIterator)}
	queryExec.resultsIter[ccname] = map[string]*mockResultsIterator{"goodquery": {kvs: kvs}}

	queryExec.txsim = ctxt.Value(TXSimulatorKey).(ledger.TxSimulator)
	ctxt = context.WithValue(ctxt, TXSimulatorKey, queryExec)

	//create the response
	queryStateNextFunc := func(reqMsg *pb.ChaincodeMessage) *pb.ChaincodeMessage {
		qr := &pb.QueryResponse{}
		proto.Unmarshal(reqMsg.Payload, qr)
		return &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_QUERY_STATE_NEXT, Payload: putils.MarshalOrPanic(&pb.QueryStateNext{Id: qr.Id}), Txid: txid, ChannelId: chainID}
	}
	queryStateCloseFunc := func(reqMsg *pb.ChaincodeMessage) *pb.ChaincodeMessage {
		qr := &pb.QueryResponse{}
		proto.Unmarshal(reqMsg.Payload, qr)
		return &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_QUERY_STATE_CLOSE, Payload: putils.MarshalOrPanic(&pb.QueryStateClose{Id: qr.Id}), Txid: txid, ChannelId: chainID}
	}

	var mkpeer []*mockpeer.MockResponse

	mkpeer = append(mkpeer, &mockpeer.MockResponse{
		RecvMsg: &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_TRANSACTION},
		RespMsg: &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_GET_QUERY_RESULT, Payload: putils.MarshalOrPanic(&pb.GetQueryResult{Collection: "", Query: "goodquery"}), Txid: txid, ChannelId: chainID},
	})

	if collection == "" {
		mkpeer = append(mkpeer, &mockpeer.MockResponse{
			RecvMsg: &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_RESPONSE},
			RespMsg: queryStateNextFunc,
		})
		mkpeer = append(mkpeer, &mockpeer.MockResponse{
			RecvMsg: &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_ERROR},
			RespMsg: queryStateCloseFunc,
		})
		mkpeer = append(mkpeer, &mockpeer.MockResponse{
			RecvMsg: &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_RESPONSE},
			RespMsg: &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_COMPLETED, Payload: putils.MarshalOrPanic(&pb.Response{Status: shim.OK, Payload: []byte("OK")}), Txid: txid, ChannelId: chainID},
		})
	} else {
		// Get query results on private data is not yet implemented.
		mkpeer = append(mkpeer, &mockpeer.MockResponse{
			RecvMsg: &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_ERROR},
			RespMsg: &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_COMPLETED, Payload: putils.MarshalOrPanic(&pb.Response{Status: shim.ERROR, Message: "Not Yet Supported"}), Txid: txid, ChannelId: chainID},
		})
	}

	respSet := &mockpeer.MockResponseSet{
		DoneFunc:  errorFunc,
		ErrorFunc: nil,
		Responses: mkpeer,
	}

	cccid := ccprovider.NewCCContext(chainID, ccname, "0", txid, false, sprop, prop)
	if collection == "" {
		execCC(t, ctxt, ccSide, cccid, false, false, done, cis, respSet, chaincodeSupport)
	} else {
		execCC(t, ctxt, ccSide, cccid, false, true, done, cis, respSet, chaincodeSupport)
	}

	endTx(t, cccid, txsim, cis)

	return nil
}

func getHistory(t *testing.T, chainID, ccname string, ccSide *mockpeer.MockCCComm, chaincodeSupport *ChaincodeSupport) error {
	done := setuperror()

	errorFunc := func(ind int, err error) {
		done <- err
	}

	chaincodeID := &pb.ChaincodeID{Name: ccname, Version: "0"}
	ci := &pb.ChaincodeInput{Args: [][]byte{[]byte("invoke"), []byte("A"), []byte("B"), []byte("10")}, Decorations: nil}
	cis := &pb.ChaincodeInvocationSpec{ChaincodeSpec: &pb.ChaincodeSpec{Type: pb.ChaincodeSpec_Type(pb.ChaincodeSpec_Type_value["GOLANG"]), ChaincodeId: chaincodeID, Input: ci}}
	txid := util.GenerateUUID()
	ctxt, txsim, sprop, prop := startTx(t, chainID, cis, txid)

	mockAclProvider.Reset()
	mockAclProvider.On("CheckACL", resources.Lscc_GetDeploymentSpec, chainID, sprop).Return(nil)
	mockAclProvider.On("CheckACL", resources.Lscc_GetChaincodeData, chainID, sprop).Return(nil)
	mockAclProvider.On("CheckACL", resources.Peer_Propose, chainID, sprop).Return(nil)

	kvs := make([]*plgr.KV, 1000)
	for i := 0; i < 1000; i++ {
		kvs[i] = &plgr.KV{Namespace: chainID, Key: fmt.Sprintf("%d", i), Value: []byte(fmt.Sprintf("%d", i))}
	}

	queryExec := &mockExecQuerySimulator{resultsIter: make(map[string]map[string]*mockResultsIterator)}
	queryExec.resultsIter[ccname] = map[string]*mockResultsIterator{"goodquery": {kvs: kvs}}

	queryExec.txsim = ctxt.Value(TXSimulatorKey).(ledger.TxSimulator)
	ctxt = context.WithValue(ctxt, TXSimulatorKey, queryExec)

	//create the response
	queryStateNextFunc := func(reqMsg *pb.ChaincodeMessage) *pb.ChaincodeMessage {
		qr := &pb.QueryResponse{}
		proto.Unmarshal(reqMsg.Payload, qr)
		return &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_QUERY_STATE_NEXT, Payload: putils.MarshalOrPanic(&pb.QueryStateNext{Id: qr.Id}), Txid: txid}
	}
	queryStateCloseFunc := func(reqMsg *pb.ChaincodeMessage) *pb.ChaincodeMessage {
		qr := &pb.QueryResponse{}
		proto.Unmarshal(reqMsg.Payload, qr)
		return &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_QUERY_STATE_CLOSE, Payload: putils.MarshalOrPanic(&pb.QueryStateClose{Id: qr.Id}), Txid: txid}
	}

	respSet := &mockpeer.MockResponseSet{
		DoneFunc:  errorFunc,
		ErrorFunc: nil,
		Responses: []*mockpeer.MockResponse{
			{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_TRANSACTION}, &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_GET_HISTORY_FOR_KEY, Payload: putils.MarshalOrPanic(&pb.GetQueryResult{Query: "goodquery"}), Txid: txid, ChannelId: chainID}},
			{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_RESPONSE}, queryStateNextFunc},
			{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_RESPONSE}, queryStateNextFunc},
			{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_ERROR}, queryStateCloseFunc},
			{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_RESPONSE}, &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_COMPLETED, Payload: putils.MarshalOrPanic(&pb.Response{Status: shim.OK, Payload: []byte("OK")}), Txid: txid, ChannelId: chainID}},
		},
	}

	cccid := ccprovider.NewCCContext(chainID, ccname, "0", txid, false, sprop, prop)
	execCC(t, ctxt, ccSide, cccid, false, false, done, cis, respSet, chaincodeSupport)

	endTx(t, cccid, txsim, cis)

	return nil
}

func getLaunchConfigs(t *testing.T, cr *ContainerRuntime) {
	gt := NewGomegaWithT(t)
	ccContext := ccprovider.NewCCContext("dummyChannelId", "mycc", "v0", "dummyTxid", false, nil, nil)
	lc, err := cr.LaunchConfig(ccContext.GetCanonicalName(), pb.ChaincodeSpec_GOLANG)
	if err != nil {
		t.Fatalf("calling getLaunchConfigs() failed with error %s", err)
	}
	args := lc.Args
	envs := lc.Envs
	filesToUpload := lc.Files

	if len(args) != 2 {
		t.Fatalf("calling getLaunchConfigs() for golang chaincode should have returned an array of 2 elements for Args, but got %v", args)
	}
	if args[0] != "chaincode" || !strings.HasPrefix(args[1], "-peer.address") {
		t.Fatalf("calling getLaunchConfigs() should have returned the start command for golang chaincode, but got %v", args)
	}

	if len(envs) != 8 {
		t.Fatalf("calling getLaunchConfigs() with TLS enabled should have returned an array of 8 elements for Envs, but got %v", envs)
	}
	gt.Expect(envs).To(ContainElement("CORE_CHAINCODE_LOGGING_LEVEL=info"))
	gt.Expect(envs).To(ContainElement("CORE_CHAINCODE_LOGGING_SHIM=warning"))
	gt.Expect(envs).To(ContainElement("CORE_CHAINCODE_ID_NAME=mycc:v0"))
	gt.Expect(envs).To(ContainElement("CORE_PEER_TLS_ENABLED=true"))
	gt.Expect(envs).To(ContainElement("CORE_TLS_CLIENT_KEY_PATH=/etc/hyperledger/fabric/client.key"))
	gt.Expect(envs).To(ContainElement("CORE_TLS_CLIENT_CERT_PATH=/etc/hyperledger/fabric/client.crt"))
	gt.Expect(envs).To(ContainElement("CORE_PEER_TLS_ROOTCERT_FILE=/etc/hyperledger/fabric/peer.crt"))

	if len(filesToUpload) != 3 {
		t.Fatalf("calling getLaunchConfigs() with TLS enabled should have returned an array of 3 elements for filesToUpload, but got %v", len(filesToUpload))
	}

	cr.CertGenerator = nil // disable TLS
	lc, err = cr.LaunchConfig(ccContext.GetCanonicalName(), pb.ChaincodeSpec_NODE)
	assert.NoError(t, err)
	args = lc.Args

	if len(args) != 3 {
		t.Fatalf("calling getLaunchConfigs() for node chaincode should have returned an array of 3 elements for Args, but got %v", args)
	}

	if args[0] != "/bin/sh" || args[1] != "-c" || !strings.HasPrefix(args[2], "cd /usr/local/src; npm start -- --peer.address") {
		t.Fatalf("calling getLaunchConfigs() should have returned the start command for node.js chaincode, but got %v", args)
	}

	lc, err = cr.LaunchConfig(ccContext.GetCanonicalName(), pb.ChaincodeSpec_GOLANG)
	assert.NoError(t, err)

	envs = lc.Envs
	if len(envs) != 5 {
		t.Fatalf("calling getLaunchConfigs() with TLS disabled should have returned an array of 4 elements for Envs, but got %v", envs)
	}
	gt.Expect(envs).To(ContainElement("CORE_CHAINCODE_LOGGING_LEVEL=info"))
	gt.Expect(envs).To(ContainElement("CORE_CHAINCODE_LOGGING_SHIM=warning"))
	gt.Expect(envs).To(ContainElement("CORE_CHAINCODE_ID_NAME=mycc:v0"))
	gt.Expect(envs).To(ContainElement("CORE_PEER_TLS_ENABLED=false"))
}

//success case
func TestStartAndWaitSuccess(t *testing.T) {
	handlerRegistry := NewHandlerRegistry(false)
	fakeRuntime := &mock.Runtime{}
	fakeRuntime.StartStub = func(_ context.Context, _ *ccprovider.CCContext, _ *pb.ChaincodeDeploymentSpec) error {
		handlerRegistry.Ready("testcc:0")
		return nil
	}

	launcher := &RuntimeLauncher{
		Runtime:        fakeRuntime,
		Registry:       handlerRegistry,
		StartupTimeout: 10 * time.Second,
	}
	spec := &pb.ChaincodeSpec{Type: pb.ChaincodeSpec_Type(pb.ChaincodeSpec_Type_value["GOLANG"]), ChaincodeId: &pb.ChaincodeID{Name: "testcc", Version: "0"}}
	code := getTarGZ(t, "src/dummy/dummy.go", []byte("code"))
	cds := &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec, CodePackage: code}
	cccid := ccprovider.NewCCContext("testchannel", "testcc", "0", "landwtimertest_txid", false, nil, nil)

	//actual test - everythings good
	err := launcher.start(context.Background(), cccid, cds)
	if err != nil {
		t.Fatalf("expected success but failed with error %s", err)
	}
}

//test timeout error
func TestStartAndWaitTimeout(t *testing.T) {
	fakeRuntime := &mock.Runtime{}
	fakeRuntime.StartStub = func(_ context.Context, _ *ccprovider.CCContext, _ *pb.ChaincodeDeploymentSpec) error {
		time.Sleep(time.Second)
		return nil
	}

	launcher := &RuntimeLauncher{
		Runtime:        fakeRuntime,
		Registry:       NewHandlerRegistry(false),
		StartupTimeout: 500 * time.Millisecond,
	}
	spec := &pb.ChaincodeSpec{Type: pb.ChaincodeSpec_Type(pb.ChaincodeSpec_Type_value["GOLANG"]), ChaincodeId: &pb.ChaincodeID{Name: "testcc", Version: "0"}}
	code := getTarGZ(t, "src/dummy/dummy.go", []byte("code"))
	cds := &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec, CodePackage: code}
	cccid := ccprovider.NewCCContext("testchannel", "testcc", "0", "landwtimertest_txid", false, nil, nil)

	//the actual test - timeout 1000 > 500
	err := launcher.start(context.Background(), cccid, cds)
	if err == nil {
		t.Fatalf("expected error but succeeded")
	}
}

//test container return error
func TestStartAndWaitLaunchError(t *testing.T) {
	fakeRuntime := &mock.Runtime{}
	fakeRuntime.StartStub = func(_ context.Context, _ *ccprovider.CCContext, _ *pb.ChaincodeDeploymentSpec) error {
		return errors.New("Bad lunch; upset stomach")
	}

	launcher := &RuntimeLauncher{
		Runtime:        fakeRuntime,
		Registry:       NewHandlerRegistry(false),
		StartupTimeout: 10 * time.Second,
	}
	spec := &pb.ChaincodeSpec{Type: pb.ChaincodeSpec_Type(pb.ChaincodeSpec_Type_value["GOLANG"]), ChaincodeId: &pb.ChaincodeID{Name: "testcc", Version: "0"}}
	code := getTarGZ(t, "src/dummy/dummy.go", []byte("code"))
	cds := &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec, CodePackage: code}
	cccid := ccprovider.NewCCContext("testchannel", "testcc", "0", "landwtimertest_txid", false, nil, nil)

	//actual test - container launch gives error
	err := launcher.start(context.Background(), cccid, cds)
	if err == nil {
		t.Fatalf("expected error but succeeded")
	}
	assert.EqualError(t, err, "error starting container: Bad lunch; upset stomach")
}

func TestGetTxContextFromHandler(t *testing.T) {
	h := Handler{TXContexts: NewTransactionContexts(), SystemCCProvider: &scc.Provider{Peer: peer.Default, PeerSupport: peer.DefaultSupport, Registrar: inproccontroller.NewRegistry()}}

	chnl := "test"
	txid := "1"
	// test getTxContext for TEST channel, tx=1, msgType=IVNOKE_CHAINCODE and empty payload - empty payload => expect to return empty txContext
	txContext, _ := h.getTxContextForInvoke(chnl, "1", []byte(""), "[%s]No ledger context for %s. Sending %s", 12345, "TestCC", pb.ChaincodeMessage_ERROR)
	if txContext != nil {
		t.Fatalf("expected empty txContext for empty payload")
	}

	// mock a peer ldger for our channel
	peer.MockInitialize()

	err := peer.MockCreateChain(chnl)
	if err != nil {
		t.Fatalf("failed to create Peer Ledger %s", err)
	}

	pldgr := peer.GetLedger(chnl)

	// prepare a payload and generate a TxContext in the handler to be used in the following getTxContextFroMessage with a normal UCC
	txCtxGenerated, payload := genNewPldAndCtxFromLdgr(t, "shimTestCC", chnl, txid, pldgr, &h)

	// test getTxContext for TEST channel, tx=1, msgType=IVNOKE_CHAINCODE and non empty payload => must return a non empty txContext
	txContext, ccMsg := h.getTxContextForInvoke(chnl, txid, payload, "[%s]No ledger context for %s. Sending %s", 12345, pb.ChaincodeMessage_INVOKE_CHAINCODE.String(), pb.ChaincodeMessage_ERROR)
	if txContext == nil || ccMsg != nil || txContext != txCtxGenerated {
		t.Fatalf("expected successful txContext for non empty payload and INVOKE_CHAINCODE msgType. triggerNextStateMsg: %s.", ccMsg)
	}

	// test for another msgType (PUT_STATE) with the same payload ==> must return a non empty txContext
	txContext, ccMsg = h.isValidTxSim(chnl, txid,
		"[%s]No ledger context for %s. Sending %s", 12345, pb.ChaincodeMessage_PUT_STATE, pb.ChaincodeMessage_ERROR)
	if txContext == nil || ccMsg != nil || txContext != txCtxGenerated {
		t.Fatalf("expected successful txContext for non empty payload and PUT_STATE msgType. triggerNextStateMsg: %s.", ccMsg)
	}

	// get a new txContext for our SCC tests
	txid = "2"
	// reset channel to "" to test getting a context for an SCC without a channel
	chnl = ""
	txCtxGenerated, payload = genNewPldAndCtxFromLdgr(t, "lscc", chnl, txid, pldgr, &h)

	// test getting a TxContext with an SCC without a channel => expect to return a non empty txContext
	txContext, ccMsg = h.getTxContextForInvoke(chnl, txid, payload,
		"[%s]No ledger context for %s. Sending %s", 12345, pb.ChaincodeMessage_INVOKE_CHAINCODE, pb.ChaincodeMessage_ERROR)
	if txContext == nil || ccMsg != nil || txContext != txCtxGenerated {
		t.Fatalf("expected successful txContext for non empty payload and INVOKE_CHAINCODE msgType. triggerNextStateMsg: %s.", ccMsg)
	}

	// now reset back to non empty channel and test with an SCC
	txid = "3"
	chnl = "TEST"
	txCtxGenerated, payload = genNewPldAndCtxFromLdgr(t, "lscc", chnl, txid, pldgr, &h)

	// test getting a TxContext with an SCC with channel TEST => expect to return a non empty txContext
	txContext, ccMsg = h.getTxContextForInvoke(chnl, txid, payload,
		"[%s]No ledger context for %s. Sending %s", 12345, pb.ChaincodeMessage_INVOKE_CHAINCODE, pb.ChaincodeMessage_ERROR)
	if txContext == nil || ccMsg != nil || txContext != txCtxGenerated {
		t.Fatalf("expected successful txContext for non empty payload and INVOKE_CHAINCODE msgType. triggerNextStateMsg: %s.", ccMsg)
	}

	// now test getting a context with an empty channel and a UCC instead of an SCC
	txid = "4"
	chnl = ""
	txCtxGenerated, payload = genNewPldAndCtxFromLdgr(t, "shimTestCC", chnl, txid, pldgr, &h)
	// test getting a TxContext with an SCC with channel TEST => expect to return a non empty txContext
	txContext, ccMsg = h.getTxContextForInvoke(chnl, txid, payload,
		"[%s]No ledger context for %s. Sending %s", 12345, pb.ChaincodeMessage_INVOKE_CHAINCODE, pb.ChaincodeMessage_ERROR)
	if txContext == nil || ccMsg != nil || txContext != txCtxGenerated {
		t.Fatalf("expected successful txContext for non empty payload and INVOKE_CHAINCODE msgType. triggerNextStateMsg: %s.", ccMsg)
	}

	// new test getting a context with an empty channel without the ledger creating a new context for a UCC
	txid = "5"
	payload = genNewPld(t, "shimTestCC")
	// test getting a TxContext with an SCC with channel TEST => expect to return a non empty txContext
	txContext, ccMsg = h.getTxContextForInvoke(chnl, txid, payload,
		"[%s]No ledger context for %s. Sending %s", 12345, pb.ChaincodeMessage_INVOKE_CHAINCODE, pb.ChaincodeMessage_ERROR)
	if txContext != nil || ccMsg == nil {
		t.Fatal("expected nil txContext for non empty payload and INVOKE_CHAINCODE msgType without the ledger generating a TxContext . unexpected non nil tcContext")
	}

	// test same scenario as above but for an SCC this time
	txid = "6"
	payload = genNewPld(t, "lscc")
	// test getting a TxContext with an SCC with channel TEST => expect to return a non empty txContext
	txContext, ccMsg = h.getTxContextForInvoke(chnl, txid, payload,
		"[%s]No ledger context for %s. Sending %s", 12345, pb.ChaincodeMessage_INVOKE_CHAINCODE, pb.ChaincodeMessage_ERROR)
	if txContext != nil || ccMsg == nil {
		t.Fatal("expected nil txContext for non empty payload and INVOKE_CHAINCODE msgType without the ledger generating a TxContext . unexpected non nil tcContext")
	}
}

func genNewPldAndCtxFromLdgr(t *testing.T, ccName string, chnl string, txid string, pldgr ledger.PeerLedger, h *Handler) (*TransactionContext, []byte) {
	// create a new TxSimulator for the received txid
	txsim, err := pldgr.NewTxSimulator(txid)
	if err != nil {
		t.Fatalf("failed to create TxSimulator %s", err)
	}
	// get a context for this txsim
	ctxt := context.WithValue(context.Background(), TXSimulatorKey, txsim)
	// create a new txContext in the handler to be retrieved by the tested function (ie: getTxContextForInvoke)
	newTxCtxt, err := h.TXContexts.Create(ctxt, chnl, txid, nil, nil)
	if err != nil {
		t.Fatalf("Error creating TxContext by the handler for cc %s and channel '%s': %s", ccName, chnl, err)
	}
	if newTxCtxt == nil {
		t.Fatalf("Error creating TxContext: newTxCtxt created by the handler is nil for cc %s and channel '%s'.", ccName, chnl)
	}
	// build a new cds and payload for the CC called ccName
	payload := genNewPld(t, ccName)
	return newTxCtxt, payload
}

func genNewPld(t *testing.T, ccName string) []byte {
	// build a new cds and payload for the CC called ccName
	chaincodeID := &pb.ChaincodeID{Name: ccName, Version: "0"}
	ci := &pb.ChaincodeInput{Args: [][]byte{[]byte("deploycc")}, Decorations: nil}
	cds := &pb.ChaincodeSpec{Type: pb.ChaincodeSpec_Type(pb.ChaincodeSpec_Type_value["GOLANG"]), ChaincodeId: chaincodeID, Input: ci}
	payload, err := proto.Marshal(cds)
	if err != nil {
		t.Fatalf("failed to marshal CDS %s", err)
	}
	return payload
}

func cc2SameCC(t *testing.T, chainID, chainID2, ccname string, ccSide *mockpeer.MockCCComm, chaincodeSupport *ChaincodeSupport) {
	//first deploy the CC on chainID2
	chaincodeID := &pb.ChaincodeID{Name: ccname, Version: "0"}
	ci := &pb.ChaincodeInput{Args: [][]byte{[]byte("deploycc")}, Decorations: nil}
	cis := &pb.ChaincodeInvocationSpec{ChaincodeSpec: &pb.ChaincodeSpec{Type: pb.ChaincodeSpec_Type(pb.ChaincodeSpec_Type_value["GOLANG"]), ChaincodeId: chaincodeID, Input: ci}}

	txid := util.GenerateUUID()
	ctxt, txsim, sprop, prop := startTx(t, chainID2, cis, txid)

	//setup CheckACL calls
	mockAclProvider.Reset()
	mockAclProvider.On("CheckACL", resources.Peer_ChaincodeToChaincode, chainID2, sprop).Return(nil)
	mockAclProvider.On("CheckACL", resources.Lscc_GetDeploymentSpec, chainID2, sprop).Return(nil)
	mockAclProvider.On("CheckACL", resources.Lscc_GetChaincodeData, chainID2, sprop).Return(nil)
	mockAclProvider.On("CheckACL", resources.Peer_Propose, chainID2, sprop).Return(nil)

	cccid := ccprovider.NewCCContext(chainID2, ccname, "0", txid, false, sprop, prop)

	deployCC(t, ctxt, cccid, cis.ChaincodeSpec, chaincodeSupport)

	//commit
	endTx(t, cccid, txsim, cis)

	done := setuperror()

	errorFunc := func(ind int, err error) {
		done <- err
	}

	//now for the test - call the same cc on a different channel(should succeed), call the same cc on the same channel(should fail)
	//Note the error "Another request pending for this Txid. Cannot process." in the logs under TX "cctosamecctx"
	ci = &pb.ChaincodeInput{Args: [][]byte{[]byte("invoke"), []byte("A"), []byte("B"), []byte("10")}, Decorations: nil}
	cis = &pb.ChaincodeInvocationSpec{ChaincodeSpec: &pb.ChaincodeSpec{Type: pb.ChaincodeSpec_Type(pb.ChaincodeSpec_Type_value["GOLANG"]), ChaincodeId: chaincodeID, Input: ci}}
	txid = util.GenerateUUID()
	ctxt, txsim, sprop, prop = startTx(t, chainID, cis, txid)

	mockAclProvider.On("CheckACL", resources.Peer_ChaincodeToChaincode, chainID, sprop).Return(nil)
	mockAclProvider.On("CheckACL", resources.Lscc_GetDeploymentSpec, chainID, sprop).Return(nil)
	mockAclProvider.On("CheckACL", resources.Lscc_GetChaincodeData, chainID, sprop).Return(nil)
	mockAclProvider.On("CheckACL", resources.Peer_Propose, chainID, sprop).Return(nil)

	mockAclProvider.On("CheckACL", resources.Peer_ChaincodeToChaincode, chainID2, sprop).Return(nil)
	mockAclProvider.On("CheckACL", resources.Lscc_GetDeploymentSpec, chainID2, sprop).Return(nil)
	mockAclProvider.On("CheckACL", resources.Lscc_GetChaincodeData, chainID2, sprop).Return(nil)
	mockAclProvider.On("CheckACL", resources.Peer_Propose, chainID2, sprop).Return(nil)

	txid = "cctosamecctx"
	respSet := &mockpeer.MockResponseSet{
		DoneFunc:  errorFunc,
		ErrorFunc: nil,
		Responses: []*mockpeer.MockResponse{
			{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_TRANSACTION}, &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_INVOKE_CHAINCODE, Payload: putils.MarshalOrPanic(&pb.ChaincodeSpec{ChaincodeId: &pb.ChaincodeID{Name: ccname + ":0/" + chainID2}, Input: &pb.ChaincodeInput{Args: [][]byte{{}}}}), Txid: txid, ChannelId: chainID}},
			{&pb.ChaincodeMessage{Type: pb.ChaincodeMessage_RESPONSE}, &pb.ChaincodeMessage{Type: pb.ChaincodeMessage_INVOKE_CHAINCODE, Payload: putils.MarshalOrPanic(&pb.ChaincodeSpec{ChaincodeId: &pb.ChaincodeID{Name: ccname + ":0/" + chainID}, Input: &pb.ChaincodeInput{Args: [][]byte{{}}}}), Txid: txid, ChannelId: chainID}},
		},
	}

	cccid = ccprovider.NewCCContext(chainID, ccname, "0", txid, false, sprop, prop)

	execCC(t, ctxt, ccSide, cccid, false, true, done, cis, respSet, chaincodeSupport)

	endTx(t, cccid, txsim, cis)
}

func TestCCFramework(t *testing.T) {
	//register 2 channels
	chainID := "mockchainid"
	chainID2 := "secondchain"
	chaincodeSupport, err := initMockPeer(chainID, chainID2)
	if err != nil {
		t.Fatalf("%s", err)
	}
	defer finitMockPeer(chainID, chainID2)
	//create a chaincode
	ccname := "shimTestCC"

	//starts and registers the CC
	_, ccSide := startCC(t, chainID, ccname, chaincodeSupport)
	if ccSide == nil {
		t.Fatalf("start up failed")
	}
	defer ccSide.Quit()

	//call's init and does some PUT (after doing some negative testing)
	initializeCC(t, chainID, ccname, ccSide, chaincodeSupport)

	//chaincode support should not allow dups
	handler := &Handler{chaincodeID: &pb.ChaincodeID{Name: ccname + ":0"}, SystemCCProvider: chaincodeSupport.sccp}
	if err := chaincodeSupport.HandlerRegistry.Register(handler); err == nil {
		t.Fatalf("expected re-register to fail")
	}

	//call's init and does some PUT (after doing some negative testing)
	initializeCC(t, chainID2, ccname, ccSide, chaincodeSupport)

	//call's invoke and do some GET
	invokeCC(t, chainID, ccname, ccSide, chaincodeSupport)

	// The following private data invoke is disabled because
	// this requires private data channel capability ON and hence should be present
	// in a dedicated test. One such test is present in file - executetransaction_pvtdata_test.go
	// call's invoke and do some GET/PUT/DEL on private data
	// invokePrivateDataGetPutDelCC(t, chainID, ccname, ccSide)

	//call's query state range
	getQueryStateByRange(t, "", chainID, ccname, ccSide, chaincodeSupport)

	//call's cc2cc on the same chaincode only call to chainID2 should succeed
	cc2SameCC(t, chainID, chainID2, ccname, ccSide, chaincodeSupport)

	//call's cc2cc (variation with syscc calls)
	cc2cc(t, chainID, chainID2, ccname, ccSide, chaincodeSupport)

	//call's query result
	getQueryResult(t, "", chainID, ccname, ccSide, chaincodeSupport)

	//call's history result
	getHistory(t, chainID, ccname, ccSide, chaincodeSupport)

	//just use the previous certGenerator for generating TLS key/pair
	cr := chaincodeSupport.Runtime.(*ContainerRuntime)
	getLaunchConfigs(t, cr)

	ccSide.Quit()
}
