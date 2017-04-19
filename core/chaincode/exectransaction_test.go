/*
Copyright IBM Corp. 2016 All Rights Reserved.

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

package chaincode

import (
	"fmt"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"encoding/json"
	"strings"

	"github.com/golang/protobuf/proto"

	mockpolicies "github.com/hyperledger/fabric/common/mocks/policies"
	"github.com/hyperledger/fabric/common/policies"
	"github.com/hyperledger/fabric/common/util"
	"github.com/hyperledger/fabric/core/common/ccprovider"
	"github.com/hyperledger/fabric/core/container"
	"github.com/hyperledger/fabric/core/container/ccintf"
	"github.com/hyperledger/fabric/core/ledger"
	"github.com/hyperledger/fabric/core/ledger/util/couchdb"
	"github.com/hyperledger/fabric/core/peer"
	"github.com/hyperledger/fabric/core/policy"
	"github.com/hyperledger/fabric/core/scc"
	pb "github.com/hyperledger/fabric/protos/peer"
	putils "github.com/hyperledger/fabric/protos/utils"

	"bytes"

	"github.com/hyperledger/fabric/core/ledger/ledgerconfig"
	"github.com/hyperledger/fabric/core/ledger/ledgermgmt"
	"github.com/hyperledger/fabric/msp"
	mspmgmt "github.com/hyperledger/fabric/msp/mgmt"
	"github.com/hyperledger/fabric/msp/mgmt/testtools"
	"github.com/hyperledger/fabric/protos/common"
	"github.com/spf13/viper"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

//initialize peer and start up. If security==enabled, login as vp
func initPeer(chainIDs ...string) (net.Listener, error) {
	//start clean
	finitPeer(nil, chainIDs...)

	peer.MockInitialize()

	var opts []grpc.ServerOption
	if viper.GetBool("peer.tls.enabled") {
		creds, err := credentials.NewServerTLSFromFile(viper.GetString("peer.tls.cert.file"), viper.GetString("peer.tls.key.file"))
		if err != nil {
			return nil, fmt.Errorf("Failed to generate credentials %v", err)
		}
		opts = []grpc.ServerOption{grpc.Creds(creds)}
	}
	grpcServer := grpc.NewServer(opts...)

	peerAddress, err := peer.GetLocalAddress()
	if err != nil {
		return nil, fmt.Errorf("Error obtaining peer address: %s", err)
	}
	lis, err := net.Listen("tcp", peerAddress)
	if err != nil {
		return nil, fmt.Errorf("Error starting peer listener %s", err)
	}

	getPeerEndpoint := func() (*pb.PeerEndpoint, error) {
		return &pb.PeerEndpoint{Id: &pb.PeerID{Name: "testpeer"}, Address: peerAddress}, nil
	}

	ccStartupTimeout := time.Duration(chaincodeStartupTimeoutDefault) * time.Millisecond
	pb.RegisterChaincodeSupportServer(grpcServer, NewChaincodeSupport(getPeerEndpoint, false, ccStartupTimeout))

	// Mock policy checker
	policy.RegisterPolicyCheckerFactory(&mockPolicyCheckerFactory{})

	scc.RegisterSysCCs()

	for _, id := range chainIDs {
		scc.DeDeploySysCCs(id)
		if err = peer.MockCreateChain(id); err != nil {
			closeListenerAndSleep(lis)
			return nil, err
		}
		scc.DeploySysCCs(id)
	}

	go grpcServer.Serve(lis)

	return lis, nil
}

func finitPeer(lis net.Listener, chainIDs ...string) {
	if lis != nil {
		for _, c := range chainIDs {
			scc.DeDeploySysCCs(c)
			if lgr := peer.GetLedger(c); lgr != nil {
				lgr.Close()
			}
		}
		closeListenerAndSleep(lis)
	}
	ledgermgmt.CleanupTestEnv()
	ledgerPath := viper.GetString("peer.fileSystemPath")
	os.RemoveAll(ledgerPath)
	os.RemoveAll(filepath.Join(os.TempDir(), "hyperledger"))

	//if couchdb is enabled, then cleanup the test couchdb
	if ledgerconfig.IsCouchDBEnabled() == true {

		chainID := util.GetTestChainID()

		connectURL := viper.GetString("ledger.state.couchDBConfig.couchDBAddress")
		username := viper.GetString("ledger.state.couchDBConfig.username")
		password := viper.GetString("ledger.state.couchDBConfig.password")
		maxRetries := viper.GetInt("ledger.state.couchDBConfig.maxRetries")
		maxRetriesOnStartup := viper.GetInt("ledger.state.couchDBConfig.maxRetriesOnStartup")
		requestTimeout := viper.GetDuration("ledger.state.couchDBConfig.requestTimeout")

		couchInstance, _ := couchdb.CreateCouchInstance(connectURL, username, password, maxRetries, maxRetriesOnStartup, requestTimeout)
		db := couchdb.CouchDatabase{CouchInstance: *couchInstance, DBName: chainID}
		//drop the test database
		db.DropDatabase()

	}
}

func startTxSimulation(ctxt context.Context, chainID string) (context.Context, ledger.TxSimulator, error) {
	lgr := peer.GetLedger(chainID)
	txsim, err := lgr.NewTxSimulator()
	if err != nil {
		return nil, nil, err
	}
	historyQueryExecutor, err := lgr.NewHistoryQueryExecutor()
	if err != nil {
		return nil, nil, err
	}

	ctxt = context.WithValue(ctxt, TXSimulatorKey, txsim)
	ctxt = context.WithValue(ctxt, HistoryQueryExecutorKey, historyQueryExecutor)
	return ctxt, txsim, nil
}

func endTxSimulationCDS(chainID string, _ string, txsim ledger.TxSimulator, payload []byte, commit bool, cds *pb.ChaincodeDeploymentSpec, blockNumber uint64) error {
	// get serialized version of the signer
	ss, err := signer.Serialize()
	if err != nil {
		return err
	}
	// get a proposal - we need it to get a transaction
	prop, _, err := putils.CreateDeployProposalFromCDS(chainID, cds, ss, nil, nil, nil)
	if err != nil {
		return err
	}

	return endTxSimulation(chainID, txsim, payload, commit, prop, blockNumber)
}

func endTxSimulationCIS(chainID string, _ string, txsim ledger.TxSimulator, payload []byte, commit bool, cis *pb.ChaincodeInvocationSpec, blockNumber uint64) error {
	// get serialized version of the signer
	ss, err := signer.Serialize()
	if err != nil {
		return err
	}
	// get a proposal - we need it to get a transaction
	prop, _, err := putils.CreateProposalFromCIS(common.HeaderType_ENDORSER_TRANSACTION, chainID, cis, ss)
	if err != nil {
		return err
	}

	return endTxSimulation(chainID, txsim, payload, commit, prop, blockNumber)
}

//getting a crash from ledger.Commit when doing concurrent invokes
//It is likely intentional that ledger.Commit is serial (ie, the real
//Committer will invoke this serially on each block). Mimic that here
//by forcing serialization of the ledger.Commit call.
//
//NOTE-this should NOT have any effect on the older serial tests.
//This affects only the tests in concurrent_test.go which call these
//concurrently (100 concurrent invokes followed by 100 concurrent queries)
var _commitLock_ sync.Mutex

func endTxSimulation(chainID string, txsim ledger.TxSimulator, _ []byte, commit bool, prop *pb.Proposal, blockNumber uint64) error {
	txsim.Done()
	if lgr := peer.GetLedger(chainID); lgr != nil {
		if commit {
			var txSimulationResults []byte
			var err error

			//get simulation results
			if txSimulationResults, err = txsim.GetTxSimulationResults(); err != nil {
				return err
			}

			// assemble a (signed) proposal response message
			resp, err := putils.CreateProposalResponse(prop.Header, prop.Payload, &pb.Response{Status: 200}, txSimulationResults, nil, nil, signer)
			if err != nil {
				return err
			}

			// get the envelope
			env, err := putils.CreateSignedTx(prop, signer, resp)
			if err != nil {
				return err
			}

			envBytes, err := putils.GetBytesEnvelope(env)
			if err != nil {
				return err
			}

			//create the block with 1 transaction
			block := common.NewBlock(blockNumber, []byte{})
			block.Data.Data = [][]byte{envBytes}
			//commit the block

			//see comment on _commitLock_
			_commitLock_.Lock()
			defer _commitLock_.Unlock()
			if err := lgr.Commit(block); err != nil {
				return err
			}
		}
	}

	return nil
}

// Build a chaincode.
func getDeploymentSpec(_ context.Context, spec *pb.ChaincodeSpec) (*pb.ChaincodeDeploymentSpec, error) {
	fmt.Printf("getting deployment spec for chaincode spec: %v\n", spec)
	codePackageBytes, err := container.GetChaincodePackageBytes(spec)
	if err != nil {
		return nil, err
	}
	cdDeploymentSpec := &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec, CodePackage: codePackageBytes}
	return cdDeploymentSpec, nil
}

//getDeployLSCCSpec gets the spec for the chaincode deployment to be sent to LSCC
func getDeployLSCCSpec(chainID string, cds *pb.ChaincodeDeploymentSpec) (*pb.ChaincodeInvocationSpec, error) {
	b, err := proto.Marshal(cds)
	if err != nil {
		return nil, err
	}

	sysCCVers := util.GetSysCCVersion()

	//wrap the deployment in an invocation spec to lscc...
	lsccSpec := &pb.ChaincodeInvocationSpec{ChaincodeSpec: &pb.ChaincodeSpec{Type: pb.ChaincodeSpec_GOLANG, ChaincodeId: &pb.ChaincodeID{Name: "lscc", Version: sysCCVers}, Input: &pb.ChaincodeInput{Args: [][]byte{[]byte("deploy"), []byte(chainID), b}}}}

	return lsccSpec, nil
}

// Deploy a chaincode - i.e., build and initialize.
func deploy(ctx context.Context, cccid *ccprovider.CCContext, spec *pb.ChaincodeSpec, blockNumber uint64) (b []byte, err error) {
	// First build and get the deployment spec
	cdDeploymentSpec, err := getDeploymentSpec(ctx, spec)
	if err != nil {
		return nil, err
	}

	return deploy2(ctx, cccid, cdDeploymentSpec, blockNumber)
}

func deploy2(ctx context.Context, cccid *ccprovider.CCContext, chaincodeDeploymentSpec *pb.ChaincodeDeploymentSpec, blockNumber uint64) (b []byte, err error) {
	cis, err := getDeployLSCCSpec(cccid.ChainID, chaincodeDeploymentSpec)
	if err != nil {
		return nil, fmt.Errorf("Error creating lscc spec : %s\n", err)
	}

	ctx, txsim, err := startTxSimulation(ctx, cccid.ChainID)
	if err != nil {
		return nil, fmt.Errorf("Failed to get handle to simulator: %s ", err)
	}

	uuid := util.GenerateUUID()

	cccid.TxID = uuid

	defer func() {
		//no error, lets try commit
		if err == nil {
			//capture returned error from commit
			err = endTxSimulationCDS(cccid.ChainID, uuid, txsim, []byte("deployed"), true, chaincodeDeploymentSpec, blockNumber)
		} else {
			//there was an error, just close simulation and return that
			endTxSimulationCDS(cccid.ChainID, uuid, txsim, []byte("deployed"), false, chaincodeDeploymentSpec, blockNumber)
		}
	}()

	//ignore existence errors
	ccprovider.PutChaincodeIntoFS(chaincodeDeploymentSpec)

	sysCCVers := util.GetSysCCVersion()
	sprop, prop := putils.MockSignedEndorserProposalOrPanic(cccid.ChainID, cis.ChaincodeSpec, []byte("Admin"), []byte("msg1"))
	lsccid := ccprovider.NewCCContext(cccid.ChainID, cis.ChaincodeSpec.ChaincodeId.Name, sysCCVers, uuid, true, sprop, prop)

	//write to lscc
	if _, _, err = ExecuteWithErrorFilter(ctx, lsccid, cis); err != nil {
		return nil, fmt.Errorf("Error deploying chaincode (1): %s", err)
	}

	if b, _, err = ExecuteWithErrorFilter(ctx, cccid, chaincodeDeploymentSpec); err != nil {
		return nil, fmt.Errorf("Error deploying chaincode(2): %s", err)
	}

	return b, nil
}

// Invoke a chaincode.
func invoke(ctx context.Context, chainID string, spec *pb.ChaincodeSpec, blockNumber uint64, creator []byte) (ccevt *pb.ChaincodeEvent, uuid string, retval []byte, err error) {
	return invokeWithVersion(ctx, chainID, spec.GetChaincodeId().Version, spec, blockNumber, creator)
}

// Invoke a chaincode with version (needed for upgrade)
func invokeWithVersion(ctx context.Context, chainID string, version string, spec *pb.ChaincodeSpec, blockNumber uint64, creator []byte) (ccevt *pb.ChaincodeEvent, uuid string, retval []byte, err error) {
	cdInvocationSpec := &pb.ChaincodeInvocationSpec{ChaincodeSpec: spec}

	// Now create the Transactions message and send to Peer.
	uuid = util.GenerateUUID()

	var txsim ledger.TxSimulator
	ctx, txsim, err = startTxSimulation(ctx, chainID)
	if err != nil {
		return nil, uuid, nil, fmt.Errorf("Failed to get handle to simulator: %s ", err)
	}

	defer func() {
		//no error, lets try commit
		if err == nil {
			//capture returned error from commit
			err = endTxSimulationCIS(chainID, uuid, txsim, []byte("invoke"), true, cdInvocationSpec, blockNumber)
		} else {
			//there was an error, just close simulation and return that
			endTxSimulationCIS(chainID, uuid, txsim, []byte("invoke"), false, cdInvocationSpec, blockNumber)
		}
	}()

	if len(creator) == 0 {
		creator = []byte("Admin")
	}
	sprop, prop := putils.MockSignedEndorserProposalOrPanic(chainID, spec, creator, []byte("msg1"))
	cccid := ccprovider.NewCCContext(chainID, cdInvocationSpec.ChaincodeSpec.ChaincodeId.Name, version, uuid, false, sprop, prop)
	retval, ccevt, err = ExecuteWithErrorFilter(ctx, cccid, cdInvocationSpec)
	if err != nil {
		return nil, uuid, nil, fmt.Errorf("Error invoking chaincode: %s", err)
	}

	return ccevt, uuid, retval, err
}

func closeListenerAndSleep(l net.Listener) {
	if l != nil {
		l.Close()
		time.Sleep(2 * time.Second)
	}
}

func executeDeployTransaction(t *testing.T, chainID string, name string, url string) {
	lis, err := initPeer(chainID)
	if err != nil {
		t.Fail()
		t.Logf("Error creating peer: %s", err)
	}

	defer finitPeer(lis, chainID)

	var ctxt = context.Background()

	f := "init"
	args := util.ToChaincodeArgs(f, "a", "100", "b", "200")
	spec := &pb.ChaincodeSpec{Type: 1, ChaincodeId: &pb.ChaincodeID{Name: name, Path: url, Version: "0"}, Input: &pb.ChaincodeInput{Args: args}}

	cccid := ccprovider.NewCCContext(chainID, name, "0", "", false, nil, nil)

	_, err = deploy(ctxt, cccid, spec, 0)

	cID := spec.ChaincodeId.Name
	if err != nil {
		theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec})
		t.Fail()
		t.Logf("Error deploying <%s>: %s", cID, err)
		return
	}

	theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec})
}

// chaincodeQueryChaincode function
func _(chainID string, _ string) error {
	var ctxt = context.Background()

	// Deploy first chaincode
	url1 := "github.com/hyperledger/fabric/examples/chaincode/go/chaincode_example02"

	cID1 := &pb.ChaincodeID{Name: "example02", Path: url1, Version: "0"}
	f := "init"
	args := util.ToChaincodeArgs(f, "a", "100", "b", "200")

	spec1 := &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID1, Input: &pb.ChaincodeInput{Args: args}}

	cccid1 := ccprovider.NewCCContext(chainID, "example02", "0", "", false, nil, nil)

	var nextBlockNumber uint64

	_, err := deploy(ctxt, cccid1, spec1, nextBlockNumber)
	nextBlockNumber++

	ccID1 := spec1.ChaincodeId.Name
	if err != nil {
		theChaincodeSupport.Stop(ctxt, cccid1, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec1})
		return fmt.Errorf("Error initializing chaincode %s(%s)", ccID1, err)
	}

	time.Sleep(time.Second)

	// Deploy second chaincode
	url2 := "github.com/hyperledger/fabric/examples/chaincode/go/chaincode_example05"

	cID2 := &pb.ChaincodeID{Name: "example05", Path: url2, Version: "0"}
	f = "init"
	args = util.ToChaincodeArgs(f, "sum", "0")

	spec2 := &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID2, Input: &pb.ChaincodeInput{Args: args}}

	cccid2 := ccprovider.NewCCContext(chainID, "example05", "0", "", false, nil, nil)

	_, err = deploy(ctxt, cccid2, spec2, nextBlockNumber)
	nextBlockNumber++
	ccID2 := spec2.ChaincodeId.Name
	if err != nil {
		theChaincodeSupport.Stop(ctxt, cccid1, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec1})
		theChaincodeSupport.Stop(ctxt, cccid2, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec2})
		return fmt.Errorf("Error initializing chaincode %s(%s)", ccID2, err)
	}

	time.Sleep(time.Second)

	// Invoke second chaincode, which will inturn query the first chaincode
	f = "invoke"
	args = util.ToChaincodeArgs(f, ccID1, "sum")

	spec2 = &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID2, Input: &pb.ChaincodeInput{Args: args}}
	// Invoke chaincode
	var retVal []byte
	_, _, retVal, err = invoke(ctxt, chainID, spec2, nextBlockNumber, []byte("Alice"))
	nextBlockNumber++

	if err != nil {
		theChaincodeSupport.Stop(ctxt, cccid1, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec1})
		theChaincodeSupport.Stop(ctxt, cccid2, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec2})
		return fmt.Errorf("Error invoking <%s>: %s", ccID2, err)
	}

	// Check the return value
	result, err := strconv.Atoi(string(retVal))
	if err != nil || result != 300 {
		theChaincodeSupport.Stop(ctxt, cccid1, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec1})
		theChaincodeSupport.Stop(ctxt, cccid2, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec2})
		return fmt.Errorf("Incorrect final state after transaction for <%s>: %s", ccID1, err)
	}

	// Query second chaincode, which will inturn query the first chaincode
	f = "query"
	args = util.ToChaincodeArgs(f, ccID1, "sum")

	spec2 = &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID2, Input: &pb.ChaincodeInput{Args: args}}
	// Invoke chaincode
	_, _, retVal, err = invoke(ctxt, chainID, spec2, nextBlockNumber, []byte("Alice"))

	if err != nil {
		theChaincodeSupport.Stop(ctxt, cccid1, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec1})
		theChaincodeSupport.Stop(ctxt, cccid2, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec2})
		return fmt.Errorf("Error querying <%s>: %s", ccID2, err)
	}

	// Check the return value
	result, err = strconv.Atoi(string(retVal))
	if err != nil || result != 300 {
		theChaincodeSupport.Stop(ctxt, cccid1, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec1})
		theChaincodeSupport.Stop(ctxt, cccid2, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec2})
		return fmt.Errorf("Incorrect final value after query for <%s>: %s", ccID1, err)
	}

	theChaincodeSupport.Stop(ctxt, cccid1, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec1})
	theChaincodeSupport.Stop(ctxt, cccid2, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec2})

	return nil
}

// Disable this temporarily.
// TODO: Need to enable this after update chaincode interface of chaincode repo.
// Test deploy of a transaction with a chaincode over HTTP.
//func TestHTTPExecuteDeployTransaction(t *testing.T) {
//	chainID := util.GetTestChainID()

//	// The chaincode used here cannot be from the fabric repo
//	// itself or it won't be downloaded because it will be found
//	// in GOPATH, which would defeat the test
//	executeDeployTransaction(t, chainID, "example01", "http://gopkg.in/mastersingh24/fabric-test-resources.v1")
//}

// Check the correctness of the final state after transaction execution.
func checkFinalState(cccid *ccprovider.CCContext) error {
	_, txsim, err := startTxSimulation(context.Background(), cccid.ChainID)
	if err != nil {
		return fmt.Errorf("Failed to get handle to simulator: %s ", err)
	}

	defer txsim.Done()

	cName := cccid.GetCanonicalName()

	// Invoke ledger to get state
	var Aval, Bval int
	resbytes, resErr := txsim.GetState(cccid.Name, "a")
	if resErr != nil {
		return fmt.Errorf("Error retrieving state from ledger for <%s>: %s", cName, resErr)
	}
	fmt.Printf("Got string: %s\n", string(resbytes))
	Aval, resErr = strconv.Atoi(string(resbytes))
	if resErr != nil {
		return fmt.Errorf("Error retrieving state from ledger for <%s>: %s", cName, resErr)
	}
	if Aval != 90 {
		return fmt.Errorf("Incorrect result. Aval %d != 90 <%s>", Aval, cName)
	}

	resbytes, resErr = txsim.GetState(cccid.Name, "b")
	if resErr != nil {
		return fmt.Errorf("Error retrieving state from ledger for <%s>: %s", cName, resErr)
	}
	Bval, resErr = strconv.Atoi(string(resbytes))
	if resErr != nil {
		return fmt.Errorf("Error retrieving state from ledger for <%s>: %s", cName, resErr)
	}
	if Bval != 210 {
		return fmt.Errorf("Incorrect result. Bval %d != 210 <%s>", Bval, cName)
	}

	// Success
	fmt.Printf("Aval = %d, Bval = %d\n", Aval, Bval)
	return nil
}

// Invoke chaincode_example02
func invokeExample02Transaction(ctxt context.Context, cccid *ccprovider.CCContext, cID *pb.ChaincodeID, chaincodeType pb.ChaincodeSpec_Type, args []string, destroyImage bool) error {

	var nextBlockNumber uint64

	f := "init"
	argsDeploy := util.ToChaincodeArgs(f, "a", "100", "b", "200")
	spec := &pb.ChaincodeSpec{Type: chaincodeType, ChaincodeId: cID, Input: &pb.ChaincodeInput{Args: argsDeploy}}
	_, err := deploy(ctxt, cccid, spec, nextBlockNumber)
	nextBlockNumber++
	ccID := spec.ChaincodeId.Name
	if err != nil {
		return fmt.Errorf("Error deploying <%s>: %s", ccID, err)
	}

	time.Sleep(time.Second)

	if destroyImage {
		theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec})
		dir := container.DestroyImageReq{CCID: ccintf.CCID{ChaincodeSpec: spec, NetworkID: theChaincodeSupport.peerNetworkID, PeerID: theChaincodeSupport.peerID, ChainID: cccid.ChainID}, Force: true, NoPrune: true}

		_, err = container.VMCProcess(ctxt, container.DOCKER, dir)
		if err != nil {
			err = fmt.Errorf("Error destroying image: %s", err)
			return err
		}
	}

	f = "invoke"
	invokeArgs := append([]string{f}, args...)
	spec = &pb.ChaincodeSpec{ChaincodeId: cID, Input: &pb.ChaincodeInput{Args: util.ToChaincodeArgs(invokeArgs...)}}
	_, uuid, _, err := invoke(ctxt, cccid.ChainID, spec, nextBlockNumber, nil)
	nextBlockNumber++
	if err != nil {
		return fmt.Errorf("Error invoking <%s>: %s", cccid.Name, err)
	}

	cccid.TxID = uuid
	err = checkFinalState(cccid)
	if err != nil {
		return fmt.Errorf("Incorrect final state after transaction for <%s>: %s", ccID, err)
	}

	// Test for delete state
	f = "delete"
	delArgs := util.ToChaincodeArgs(f, "a")
	spec = &pb.ChaincodeSpec{ChaincodeId: cID, Input: &pb.ChaincodeInput{Args: delArgs}}
	_, _, _, err = invoke(ctxt, cccid.ChainID, spec, nextBlockNumber, nil)
	if err != nil {
		return fmt.Errorf("Error deleting state in <%s>: %s", cccid.Name, err)
	}

	return nil
}

const (
	chaincodeExample02GolangPath = "github.com/hyperledger/fabric/examples/chaincode/go/chaincode_example02"
	chaincodeExample02JavaPath   = "../../examples/chaincode/java/chaincode_example02"
	chaincodeExample06JavaPath   = "../../examples/chaincode/java/chaincode_example06"
)

func runChaincodeInvokeChaincode(t *testing.T, chainID1 string, chainID2 string, _ string) (err error) {
	var ctxt = context.Background()

	// Deploy first chaincode
	url1 := "github.com/hyperledger/fabric/examples/chaincode/go/chaincode_example02"

	cID1 := &pb.ChaincodeID{Name: "example02", Path: url1, Version: "0"}
	f := "init"
	args := util.ToChaincodeArgs(f, "a", "100", "b", "200")

	spec1 := &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID1, Input: &pb.ChaincodeInput{Args: args}}

	sProp, prop := putils.MockSignedEndorserProposalOrPanic(chainID1, spec1, []byte([]byte("Alice")), nil)
	cccid1 := ccprovider.NewCCContext(chainID1, "example02", "0", "", false, sProp, prop)

	var nextBlockNumber uint64

	_, err = deploy(ctxt, cccid1, spec1, nextBlockNumber)
	nextBlockNumber++
	ccID1 := spec1.ChaincodeId.Name
	if err != nil {
		t.Fail()
		t.Logf("Error initializing chaincode %s(%s)", ccID1, err)
		theChaincodeSupport.Stop(ctxt, cccid1, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec1})
		return
	}

	t.Logf("deployed chaincode_example02 got cID1:% s,\n ccID1:% s", cID1, ccID1)

	time.Sleep(time.Second)

	// Deploy second chaincode
	url2 := "github.com/hyperledger/fabric/examples/chaincode/go/chaincode_example04"

	cID2 := &pb.ChaincodeID{Name: "example04", Path: url2, Version: "0"}
	f = "init"
	args = util.ToChaincodeArgs(f, "e", "0")

	spec2 := &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID2, Input: &pb.ChaincodeInput{Args: args}}

	sProp, prop = putils.MockSignedEndorserProposalOrPanic(chainID1, spec2, []byte([]byte("Alice")), nil)
	cccid2 := ccprovider.NewCCContext(chainID1, "example04", "0", "", false, sProp, prop)

	_, err = deploy(ctxt, cccid2, spec2, nextBlockNumber)
	nextBlockNumber++
	ccID2 := spec2.ChaincodeId.Name
	if err != nil {
		t.Fail()
		t.Logf("Error initializing chaincode %s(%s)", ccID2, err)
		theChaincodeSupport.Stop(ctxt, cccid1, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec1})
		theChaincodeSupport.Stop(ctxt, cccid2, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec2})
		return
	}

	time.Sleep(time.Second)

	// Invoke second chaincode passing the first chaincode's name as first param,
	// which will inturn invoke the first chaincode
	f = "invoke"
	cid := spec1.ChaincodeId.Name
	args = util.ToChaincodeArgs(f, cid, "e", "1")

	spec2 = &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID2, Input: &pb.ChaincodeInput{Args: args}}
	// Invoke chaincode
	var uuid string
	_, uuid, _, err = invoke(ctxt, chainID1, spec2, nextBlockNumber, []byte("Alice"))

	if err != nil {
		t.Fail()
		t.Logf("Error invoking <%s>: %s", ccID2, err)
		theChaincodeSupport.Stop(ctxt, cccid1, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec1})
		theChaincodeSupport.Stop(ctxt, cccid2, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec2})
		return
	}

	cccid1.TxID = uuid

	// Check the state in the ledger
	err = checkFinalState(cccid1)
	if err != nil {
		t.Fail()
		t.Logf("Incorrect final state after transaction for <%s>: %s", ccID1, err)
		theChaincodeSupport.Stop(ctxt, cccid1, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec1})
		theChaincodeSupport.Stop(ctxt, cccid2, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec2})
		return
	}

	// Change the policies of the two channels in such a way:
	// 1. Alice has reader access to both the channels.
	// 2. Bob has access only to chainID2.
	// Therefore the chaincode invocation should fail.
	pm := peer.GetPolicyManager(chainID1)
	pm.(*mockpolicies.Manager).PolicyMap = map[string]policies.Policy{
		policies.ChannelApplicationWriters: &CreatorPolicy{Creators: [][]byte{[]byte("Alice")}},
	}

	pm = peer.GetPolicyManager(chainID2)
	pm.(*mockpolicies.Manager).PolicyMap = map[string]policies.Policy{
		policies.ChannelApplicationWriters: &CreatorPolicy{Creators: [][]byte{[]byte("Alice"), []byte("Bob")}},
	}

	url2 = "github.com/hyperledger/fabric/examples/chaincode/go/chaincode_example04"

	cID2 = &pb.ChaincodeID{Name: "example04", Path: url2, Version: "0"}
	f = "init"
	args = util.ToChaincodeArgs(f, "e", "0")

	spec3 := &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID2, Input: &pb.ChaincodeInput{Args: args}}
	sProp, prop = putils.MockSignedEndorserProposalOrPanic(chainID2, spec3, []byte([]byte("Alice")), nil)
	cccid3 := ccprovider.NewCCContext(chainID2, "example04", "0", "", false, sProp, prop)

	_, err = deploy(ctxt, cccid3, spec3, 0)
	chaincodeID2 := spec2.ChaincodeId.Name
	if err != nil {
		t.Fail()
		t.Logf("Error initializing chaincode %s(%s)", chaincodeID2, err)
		theChaincodeSupport.Stop(ctxt, cccid1, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec1})
		theChaincodeSupport.Stop(ctxt, cccid2, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec2})
		theChaincodeSupport.Stop(ctxt, cccid3, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec3})
		return
	}

	time.Sleep(time.Second)
	//
	// - Invoke second chaincode passing the first chaincode's name as first param, which will in turn invoke the first chaincode
	f = "invoke"
	cid = spec1.ChaincodeId.Name
	args = util.ToChaincodeArgs(f, cid, "e", "1", chainID1)

	spec2 = &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID2, Input: &pb.ChaincodeInput{Args: args}}
	// Invoke chaincode
	//var uuid string

	// Bob should not be able to call
	_, _, _, err = invoke(ctxt, chainID2, spec2, 1, []byte("Bob"))
	if err == nil {
		t.Fail()
		t.Logf("Bob invoking <%s> should fail. It did not happen instead: %s", chaincodeID2, err)
		theChaincodeSupport.Stop(ctxt, cccid1, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec1})
		theChaincodeSupport.Stop(ctxt, cccid2, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec2})
		theChaincodeSupport.Stop(ctxt, cccid3, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec3})
		return
	}

	// Alice should be able to call
	_, _, _, err = invoke(ctxt, chainID2, spec2, 1, []byte("Alice"))
	if err != nil {
		t.Fail()
		t.Logf("Alice invoking <%s> should not fail. It did happen instead: %s", chaincodeID2, err)
		theChaincodeSupport.Stop(ctxt, cccid1, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec1})
		theChaincodeSupport.Stop(ctxt, cccid2, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec2})
		theChaincodeSupport.Stop(ctxt, cccid3, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec3})
		return
	}

	theChaincodeSupport.Stop(ctxt, cccid1, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec1})
	theChaincodeSupport.Stop(ctxt, cccid2, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec2})
	theChaincodeSupport.Stop(ctxt, cccid3, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec3})

	return nil
}

// Test deploy of a transaction
func TestExecuteDeployTransaction(t *testing.T) {
	//chaincoe is deployed as part of many tests. No need for a separate one for this
	t.Skip()
	chainID := util.GetTestChainID()

	executeDeployTransaction(t, chainID, "example01", "github.com/hyperledger/fabric/examples/chaincode/go/chaincode_example01")
}

// Test deploy of a transaction with a GOPATH with multiple elements
func TestGopathExecuteDeployTransaction(t *testing.T) {
	//this is no longer critical as chaincode is assembled in the client side (SDK)
	t.Skip()
	chainID := util.GetTestChainID()

	// add a trailing slash to GOPATH
	// and a couple of elements - it doesn't matter what they are
	os.Setenv("GOPATH", os.Getenv("GOPATH")+string(os.PathSeparator)+string(os.PathListSeparator)+"/tmp/foo"+string(os.PathListSeparator)+"/tmp/bar")
	executeDeployTransaction(t, chainID, "example01", "github.com/hyperledger/fabric/examples/chaincode/go/chaincode_example01")
}

func TestExecuteInvokeTransaction(t *testing.T) {

	testCases := []struct {
		chaincodeType pb.ChaincodeSpec_Type
		chaincodePath string
	}{
		{pb.ChaincodeSpec_GOLANG, chaincodeExample02GolangPath},
		{pb.ChaincodeSpec_JAVA, chaincodeExample02JavaPath},
	}

	for _, tc := range testCases {
		t.Run(tc.chaincodeType.String(), func(t *testing.T) {

			if tc.chaincodeType == pb.ChaincodeSpec_JAVA && runtime.GOARCH != "amd64" {
				t.Skip("No Java chaincode support yet on non-x86_64.")
			}

			chainID := util.GetTestChainID()

			lis, err := initPeer(chainID)
			if err != nil {
				t.Fail()
				t.Logf("Error creating peer: %s", err)
			}

			defer finitPeer(lis, chainID)

			var ctxt = context.Background()
			chaincodeName := generateChaincodeName(tc.chaincodeType)
			chaincodeVersion := "1.0.0.0"
			cccid := ccprovider.NewCCContext(chainID, chaincodeName, chaincodeVersion, "", false, nil, nil)
			ccID := &pb.ChaincodeID{Name: chaincodeName, Path: tc.chaincodePath, Version: chaincodeVersion}

			args := []string{"a", "b", "10"}
			err = invokeExample02Transaction(ctxt, cccid, ccID, tc.chaincodeType, args, true)
			if err != nil {
				t.Fail()
				t.Logf("Error invoking transaction: %s", err)
			} else {
				fmt.Print("Invoke test passed\n")
				t.Log("Invoke test passed")
			}

			theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: &pb.ChaincodeSpec{ChaincodeId: ccID}})

		})
	}

}

// Test the execution of an invalid transaction.
func TestExecuteInvokeInvalidTransaction(t *testing.T) {
	chainID := util.GetTestChainID()

	lis, err := initPeer(chainID)
	if err != nil {
		t.Fail()
		t.Logf("Error creating peer: %s", err)
	}

	defer finitPeer(lis, chainID)

	var ctxt = context.Background()

	url := "github.com/hyperledger/fabric/examples/chaincode/go/chaincode_example02"
	ccID := &pb.ChaincodeID{Name: "example02", Path: url, Version: "0"}

	cccid := ccprovider.NewCCContext(chainID, "example02", "0", "", false, nil, nil)

	//FAIL, FAIL!
	args := []string{"x", "-1"}
	err = invokeExample02Transaction(ctxt, cccid, ccID, pb.ChaincodeSpec_GOLANG, args, false)

	//this HAS to fail with expectedDeltaStringPrefix
	if err != nil {
		errStr := err.Error()
		t.Logf("Got error %s\n", errStr)
		t.Log("InvalidInvoke test passed")
		theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: &pb.ChaincodeSpec{ChaincodeId: ccID}})

		return
	}

	t.Fail()
	t.Logf("Error invoking transaction %s", err)

	theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: &pb.ChaincodeSpec{ChaincodeId: ccID}})
}

// Test the execution of a chaincode that invokes another chaincode.
func TestChaincodeInvokeChaincode(t *testing.T) {
	chainID1 := util.GetTestChainID()
	chainID2 := chainID1 + "2"

	lis, err := initPeer(chainID1, chainID2)
	if err != nil {
		t.Fail()
		t.Logf("Error creating peer: %s", err)
	}

	defer finitPeer(lis, chainID1, chainID2)

	err = runChaincodeInvokeChaincode(t, chainID1, chainID2, "")
	if err != nil {
		t.Fail()
		t.Logf("Failed chaincode invoke chaincode : %s", err)
		closeListenerAndSleep(lis)
		return
	}

	closeListenerAndSleep(lis)
}

// Test the execution of a chaincode that invokes another chaincode with wrong parameters. Should receive error from
// from the called chaincode
func TestChaincodeInvokeChaincodeErrorCase(t *testing.T) {
	chainID := util.GetTestChainID()

	lis, err := initPeer(chainID)
	if err != nil {
		t.Fail()
		t.Logf("Error creating peer: %s", err)
	}

	defer finitPeer(lis, chainID)

	var ctxt = context.Background()

	// Deploy first chaincode
	url1 := "github.com/hyperledger/fabric/examples/chaincode/go/chaincode_example02"

	cID1 := &pb.ChaincodeID{Name: "example02", Path: url1, Version: "0"}
	f := "init"
	args := util.ToChaincodeArgs(f, "a", "100", "b", "200")

	spec1 := &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID1, Input: &pb.ChaincodeInput{Args: args}}

	sProp, prop := putils.MockSignedEndorserProposalOrPanic(util.GetTestChainID(), spec1, []byte([]byte("Alice")), nil)
	cccid1 := ccprovider.NewCCContext(chainID, "example02", "0", "", false, sProp, prop)

	var nextBlockNumber uint64

	_, err = deploy(ctxt, cccid1, spec1, nextBlockNumber)
	nextBlockNumber++
	ccID1 := spec1.ChaincodeId.Name
	if err != nil {
		t.Fail()
		t.Logf("Error initializing chaincode %s(%s)", ccID1, err)
		theChaincodeSupport.Stop(ctxt, cccid1, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec1})
		return
	}

	time.Sleep(time.Second)

	// Deploy second chaincode
	url2 := "github.com/hyperledger/fabric/examples/chaincode/go/passthru"

	cID2 := &pb.ChaincodeID{Name: "pthru", Path: url2, Version: "0"}
	f = "init"
	args = util.ToChaincodeArgs(f)

	spec2 := &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID2, Input: &pb.ChaincodeInput{Args: args}}

	cccid2 := ccprovider.NewCCContext(chainID, "pthru", "0", "", false, sProp, prop)

	_, err = deploy(ctxt, cccid2, spec2, nextBlockNumber)
	nextBlockNumber++
	ccID2 := spec2.ChaincodeId.Name
	if err != nil {
		t.Fail()
		t.Logf("Error initializing chaincode %s(%s)", ccID2, err)
		theChaincodeSupport.Stop(ctxt, cccid1, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec1})
		theChaincodeSupport.Stop(ctxt, cccid2, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec2})
		return
	}

	time.Sleep(time.Second)

	// Invoke second chaincode, which will inturn invoke the first chaincode but pass bad params
	f = ccID1
	args = util.ToChaincodeArgs(f, "invoke", "a")

	spec2 = &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID2, Input: &pb.ChaincodeInput{Args: args}}
	// Invoke chaincode
	_, _, _, err = invoke(ctxt, chainID, spec2, nextBlockNumber, []byte("Alice"))

	if err == nil {
		t.Fail()
		t.Logf("Error invoking <%s>: %s", ccID2, err)
		theChaincodeSupport.Stop(ctxt, cccid1, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec1})
		theChaincodeSupport.Stop(ctxt, cccid2, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec2})
		return
	}

	if strings.Index(err.Error(), "Error invoking chaincode: Incorrect number of arguments. Expecting 3") < 0 {
		t.Fail()
		t.Logf("Unexpected error %s", err)
		theChaincodeSupport.Stop(ctxt, cccid1, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec1})
		theChaincodeSupport.Stop(ctxt, cccid2, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec2})
		return
	}

	theChaincodeSupport.Stop(ctxt, cccid1, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec1})
	theChaincodeSupport.Stop(ctxt, cccid2, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec2})
}

// Test the invocation of a transaction.
func TestQueries(t *testing.T) {

	chainID := util.GetTestChainID()

	lis, err := initPeer(chainID)
	if err != nil {
		t.Fail()
		t.Logf("Error creating peer: %s", err)
	}

	defer finitPeer(lis, chainID)

	var ctxt = context.Background()

	url := "github.com/hyperledger/fabric/examples/chaincode/go/map"
	cID := &pb.ChaincodeID{Name: "tmap", Path: url, Version: "0"}

	f := "init"
	args := util.ToChaincodeArgs(f)

	spec := &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID, Input: &pb.ChaincodeInput{Args: args}}

	cccid := ccprovider.NewCCContext(chainID, "tmap", "0", "", false, nil, nil)

	var nextBlockNumber uint64
	_, err = deploy(ctxt, cccid, spec, nextBlockNumber)
	nextBlockNumber++
	ccID := spec.ChaincodeId.Name
	if err != nil {
		t.Fail()
		t.Logf("Error initializing chaincode %s(%s)", ccID, err)
		theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec})
		return
	}

	// Add 12 marbles for testing range queries and rich queries (for capable ledgers)
	// The tests will test both range and rich queries and queries with query limits
	f = "put"
	args = util.ToChaincodeArgs(f, "marble01", "{\"docType\":\"marble\",\"name\":\"marble01\",\"color\":\"blue\",\"size\":35,\"owner\":\"tom\"}")
	spec = &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID, Input: &pb.ChaincodeInput{Args: args}}
	_, _, _, err = invoke(ctxt, chainID, spec, nextBlockNumber, nil)
	nextBlockNumber++

	if err != nil {
		t.Fail()
		t.Logf("Error invoking <%s>: %s", ccID, err)
		theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec})
		return
	}

	f = "put"
	args = util.ToChaincodeArgs(f, "marble02", "{\"docType\":\"marble\",\"name\":\"marble02\",\"color\":\"red\",\"size\":25,\"owner\":\"tom\"}")
	spec = &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID, Input: &pb.ChaincodeInput{Args: args}}
	_, _, _, err = invoke(ctxt, chainID, spec, nextBlockNumber, nil)
	nextBlockNumber++

	if err != nil {
		t.Fail()
		t.Logf("Error invoking <%s>: %s", ccID, err)
		theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec})
		return
	}

	f = "put"
	args = util.ToChaincodeArgs(f, "marble03", "{\"docType\":\"marble\",\"name\":\"marble03\",\"color\":\"green\",\"size\":15,\"owner\":\"tom\"}")
	spec = &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID, Input: &pb.ChaincodeInput{Args: args}}
	_, _, _, err = invoke(ctxt, chainID, spec, nextBlockNumber, nil)
	nextBlockNumber++
	if err != nil {
		t.Fail()
		t.Logf("Error invoking <%s>: %s", ccID, err)
		theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec})
		return
	}

	f = "put"
	args = util.ToChaincodeArgs(f, "marble04", "{\"docType\":\"marble\",\"name\":\"marble04\",\"color\":\"green\",\"size\":20,\"owner\":\"jerry\"}")
	spec = &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID, Input: &pb.ChaincodeInput{Args: args}}
	_, _, _, err = invoke(ctxt, chainID, spec, nextBlockNumber, nil)
	nextBlockNumber++
	if err != nil {
		t.Fail()
		t.Logf("Error invoking <%s>: %s", ccID, err)
		theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec})
		return
	}

	f = "put"
	args = util.ToChaincodeArgs(f, "marble05", "{\"docType\":\"marble\",\"name\":\"marble05\",\"color\":\"red\",\"size\":25,\"owner\":\"jerry\"}")
	spec = &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID, Input: &pb.ChaincodeInput{Args: args}}
	_, _, _, err = invoke(ctxt, chainID, spec, nextBlockNumber, nil)
	nextBlockNumber++
	if err != nil {
		t.Fail()
		t.Logf("Error invoking <%s>: %s", ccID, err)
		theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec})
		return
	}

	f = "put"
	args = util.ToChaincodeArgs(f, "marble06", "{\"docType\":\"marble\",\"name\":\"marble06\",\"color\":\"blue\",\"size\":35,\"owner\":\"jerry\"}")
	spec = &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID, Input: &pb.ChaincodeInput{Args: args}}
	_, _, _, err = invoke(ctxt, chainID, spec, nextBlockNumber, nil)
	nextBlockNumber++
	if err != nil {
		t.Fail()
		t.Logf("Error invoking <%s>: %s", ccID, err)
		theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec})
		return
	}

	f = "put"
	args = util.ToChaincodeArgs(f, "marble07", "{\"docType\":\"marble\",\"name\":\"marble07\",\"color\":\"yellow\",\"size\":20,\"owner\":\"jerry\"}")
	spec = &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID, Input: &pb.ChaincodeInput{Args: args}}
	_, _, _, err = invoke(ctxt, chainID, spec, nextBlockNumber, nil)
	nextBlockNumber++
	if err != nil {
		t.Fail()
		t.Logf("Error invoking <%s>: %s", ccID, err)
		theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec})
		return
	}

	f = "put"
	args = util.ToChaincodeArgs(f, "marble08", "{\"docType\":\"marble\",\"name\":\"marble08\",\"color\":\"green\",\"size\":40,\"owner\":\"jerry\"}")
	spec = &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID, Input: &pb.ChaincodeInput{Args: args}}
	_, _, _, err = invoke(ctxt, chainID, spec, nextBlockNumber, nil)
	nextBlockNumber++
	if err != nil {
		t.Fail()
		t.Logf("Error invoking <%s>: %s", ccID, err)
		theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec})
		return
	}

	f = "put"
	args = util.ToChaincodeArgs(f, "marble09", "{\"docType\":\"marble\",\"name\":\"marble09\",\"color\":\"yellow\",\"size\":10,\"owner\":\"jerry\"}")
	spec = &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID, Input: &pb.ChaincodeInput{Args: args}}
	_, _, _, err = invoke(ctxt, chainID, spec, nextBlockNumber, nil)
	nextBlockNumber++
	if err != nil {
		t.Fail()
		t.Logf("Error invoking <%s>: %s", ccID, err)
		theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec})
		return
	}

	f = "put"
	args = util.ToChaincodeArgs(f, "marble10", "{\"docType\":\"marble\",\"name\":\"marble10\",\"color\":\"red\",\"size\":20,\"owner\":\"jerry\"}")
	spec = &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID, Input: &pb.ChaincodeInput{Args: args}}
	_, _, _, err = invoke(ctxt, chainID, spec, nextBlockNumber, nil)
	nextBlockNumber++
	if err != nil {
		t.Fail()
		t.Logf("Error invoking <%s>: %s", ccID, err)
		theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec})
		return
	}

	f = "put"
	args = util.ToChaincodeArgs(f, "marble11", "{\"docType\":\"marble\",\"name\":\"marble11\",\"color\":\"green\",\"size\":40,\"owner\":\"jerry\"}")
	spec = &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID, Input: &pb.ChaincodeInput{Args: args}}
	_, _, _, err = invoke(ctxt, chainID, spec, nextBlockNumber, nil)
	nextBlockNumber++
	if err != nil {
		t.Fail()
		t.Logf("Error invoking <%s>: %s", ccID, err)
		theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec})
		return
	}

	f = "put"
	args = util.ToChaincodeArgs(f, "marble12", "{\"docType\":\"marble\",\"name\":\"marble12\",\"color\":\"red\",\"size\":30,\"owner\":\"jerry\"}")
	spec = &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID, Input: &pb.ChaincodeInput{Args: args}}
	_, _, _, err = invoke(ctxt, chainID, spec, nextBlockNumber, nil)
	nextBlockNumber++
	if err != nil {
		t.Fail()
		t.Logf("Error invoking <%s>: %s", ccID, err)
		theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec})
		return
	}

	//TODO - the following query tests for queryLimits may change due to future designs
	//       for batch "paging"

	//The following range query for "marble01" to "marble11" should return 10 marbles
	f = "keys"
	args = util.ToChaincodeArgs(f, "marble01", "marble11")

	spec = &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID, Input: &pb.ChaincodeInput{Args: args}}
	_, _, retval, err := invoke(ctxt, chainID, spec, nextBlockNumber, nil)
	nextBlockNumber++
	if err != nil {
		t.Fail()
		t.Logf("Error invoking <%s>: %s", ccID, err)
		theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec})
		return
	}

	var keys []interface{}
	err = json.Unmarshal(retval, &keys)

	//default query limit of 10000 is used, query should return all records that meet the criteria
	if len(keys) != 10 {
		t.Fail()
		t.Logf("Error detected with the range query, should have returned 10 but returned %v", len(keys))
		theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec})
		return
	}

	//Reset the query limit to 5
	viper.Set("ledger.state.queryLimit", 5)

	//The following range query for "marble01" to "marble11" should return 5 marbles due to the queryLimit
	f = "keys"
	args = util.ToChaincodeArgs(f, "marble01", "marble11")

	spec = &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID, Input: &pb.ChaincodeInput{Args: args}}
	_, _, retval, err = invoke(ctxt, chainID, spec, nextBlockNumber, nil)

	nextBlockNumber++
	if err != nil {
		t.Fail()
		t.Logf("Error invoking <%s>: %s", ccID, err)
		theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec})
		return
	}

	//unmarshal the results
	err = json.Unmarshal(retval, &keys)

	//check to see if there are 5 values
	if len(keys) != 5 {
		t.Fail()
		t.Logf("Error detected with the range query, should have returned 5 but returned %v", len(keys))
		theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec})
		return
	}

	//Reset the query limit to default
	viper.Set("ledger.state.queryLimit", 10000)

	if ledgerconfig.IsHistoryDBEnabled() == true {

		f = "put"
		args = util.ToChaincodeArgs(f, "marble12", "{\"docType\":\"marble\",\"name\":\"marble12\",\"color\":\"red\",\"size\":30,\"owner\":\"jerry\"}")
		spec = &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID, Input: &pb.ChaincodeInput{Args: args}}
		_, _, _, err = invoke(ctxt, chainID, spec, nextBlockNumber, nil)
		nextBlockNumber++
		if err != nil {
			t.Fail()
			t.Logf("Error invoking <%s>: %s", ccID, err)
			theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec})
			return
		}

		f = "put"
		args = util.ToChaincodeArgs(f, "marble12", "{\"docType\":\"marble\",\"name\":\"marble12\",\"color\":\"red\",\"size\":30,\"owner\":\"jerry\"}")
		spec = &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID, Input: &pb.ChaincodeInput{Args: args}}
		_, _, _, err = invoke(ctxt, chainID, spec, nextBlockNumber, nil)
		nextBlockNumber++
		if err != nil {
			t.Fail()
			t.Logf("Error invoking <%s>: %s", ccID, err)
			theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec})
			return
		}

		//The following history query for "marble12" should return 3 records
		f = "history"
		args = util.ToChaincodeArgs(f, "marble12")

		spec = &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID, Input: &pb.ChaincodeInput{Args: args}}
		_, _, retval, err := invoke(ctxt, chainID, spec, nextBlockNumber, nil)
		nextBlockNumber++
		if err != nil {
			t.Fail()
			t.Logf("Error invoking <%s>: %s", ccID, err)
			theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec})
			return
		}

		var history []interface{}
		err = json.Unmarshal(retval, &history)

		//default query limit of 10000 is used, query should return all records that meet the criteria
		if len(history) != 3 {
			t.Fail()
			t.Logf("Error detected with the history query, should have returned 3 but returned %v", len(keys))
			theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec})
			return
		}

	}

	if ledgerconfig.IsCouchDBEnabled() == true {

		//The following rich query for should return 9 marbles
		f = "query"
		args = util.ToChaincodeArgs(f, "{\"selector\":{\"owner\":\"jerry\"}}")

		spec = &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID, Input: &pb.ChaincodeInput{Args: args}}
		_, _, retval, err = invoke(ctxt, chainID, spec, nextBlockNumber, nil)
		nextBlockNumber++

		if err != nil {
			t.Fail()
			t.Logf("Error invoking <%s>: %s", ccID, err)
			theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec})
			return
		}

		//unmarshal the results
		err = json.Unmarshal(retval, &keys)

		//check to see if there are 9 values
		//default query limit of 10000 is used, this query is effectively unlimited
		if len(keys) != 9 {
			t.Fail()
			t.Logf("Error detected with the rich query, should have returned 9 but returned %v", len(keys))
			theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec})
			return
		}

		//Reset the query limit to 5
		viper.Set("ledger.state.queryLimit", 5)

		//The following rich query should return 5 marbles due to the queryLimit
		f = "query"
		args = util.ToChaincodeArgs(f, "{\"selector\":{\"owner\":\"jerry\"}}")

		spec = &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID, Input: &pb.ChaincodeInput{Args: args}}
		_, _, retval, err = invoke(ctxt, chainID, spec, nextBlockNumber, nil)

		if err != nil {
			t.Fail()
			t.Logf("Error invoking <%s>: %s", ccID, err)
			theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec})
			return
		}

		//unmarshal the results
		err = json.Unmarshal(retval, &keys)

		//check to see if there are 5 values
		if len(keys) != 5 {
			t.Fail()
			t.Logf("Error detected with the rich query, should have returned 5 but returned %v", len(keys))
			theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec})
			return
		}

	}

	theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec})
}

func TestGetEvent(t *testing.T) {
	chainID := util.GetTestChainID()

	lis, err := initPeer(chainID)
	if err != nil {
		t.Fail()
		t.Logf("Error creating peer: %s", err)
	}

	defer finitPeer(lis, chainID)

	var ctxt = context.Background()

	url := "github.com/hyperledger/fabric/examples/chaincode/go/eventsender"

	cID := &pb.ChaincodeID{Name: "esender", Path: url, Version: "0"}
	f := "init"
	spec := &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID, Input: &pb.ChaincodeInput{Args: util.ToChaincodeArgs(f)}}

	cccid := ccprovider.NewCCContext(chainID, "esender", "0", "", false, nil, nil)
	var nextBlockNumber uint64
	_, err = deploy(ctxt, cccid, spec, nextBlockNumber)
	nextBlockNumber++
	ccID := spec.ChaincodeId.Name
	if err != nil {
		t.Fail()
		t.Logf("Error initializing chaincode %s(%s)", ccID, err)
		theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec})
		return
	}

	time.Sleep(time.Second)

	args := util.ToChaincodeArgs("invoke", "i", "am", "satoshi")

	spec = &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID, Input: &pb.ChaincodeInput{Args: args}}

	var ccevt *pb.ChaincodeEvent
	ccevt, _, _, err = invoke(ctxt, chainID, spec, nextBlockNumber, nil)

	if err != nil {
		t.Logf("Error invoking chaincode %s(%s)", ccID, err)
		t.Fail()
	}

	if ccevt == nil {
		t.Logf("Error ccevt is nil %s(%s)", ccID, err)
		t.Fail()
	}

	if ccevt.ChaincodeId != ccID {
		t.Logf("Error ccevt id(%s) != cid(%s)", ccevt.ChaincodeId, ccID)
		t.Fail()
	}

	if strings.Index(string(ccevt.Payload), "i,am,satoshi") < 0 {
		t.Logf("Error expected event not found (%s)", string(ccevt.Payload))
		t.Fail()
	}

	theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec})
}

// Test the execution of a chaincode that queries another chaincode
// example02 implements "query" as a function in Invoke. example05 calls example02
func TestChaincodeQueryChaincodeUsingInvoke(t *testing.T) {
	//this is essentially same as the ChaincodeInvokeChaincode now that
	//we don't distinguish between Invoke and Query (there's no separate "Query")
	t.Skip()
	chainID := util.GetTestChainID()

	var peerLis net.Listener
	var err error
	if peerLis, err = initPeer(chainID); err != nil {
		t.Fail()
		t.Logf("Error registering user  %s", err)
		return
	}

	defer finitPeer(peerLis, chainID)

	var ctxt = context.Background()

	// Deploy first chaincode
	url1 := "github.com/hyperledger/fabric/examples/chaincode/go/chaincode_example02"

	cID1 := &pb.ChaincodeID{Name: "example02", Path: url1, Version: "0"}
	f := "init"
	args := util.ToChaincodeArgs(f, "a", "100", "b", "200")

	spec1 := &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID1, Input: &pb.ChaincodeInput{Args: args}}

	sProp, prop := putils.MockSignedEndorserProposalOrPanic(util.GetTestChainID(), spec1, []byte([]byte("Alice")), nil)
	cccid1 := ccprovider.NewCCContext(chainID, "example02", "0", "", false, sProp, prop)
	var nextBlockNumber uint64
	_, err = deploy(ctxt, cccid1, spec1, nextBlockNumber)
	nextBlockNumber++
	ccID1 := spec1.ChaincodeId.Name
	if err != nil {
		theChaincodeSupport.Stop(ctxt, cccid1, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec1})
		t.Fail()
		t.Logf("Error initializing chaincode %s(%s)", ccID1, err)
		return
	}

	time.Sleep(time.Second)

	// Deploy second chaincode
	url2 := "github.com/hyperledger/fabric/examples/chaincode/go/chaincode_example05"

	cID2 := &pb.ChaincodeID{Name: "example05", Path: url2, Version: "0"}
	f = "init"
	args = util.ToChaincodeArgs(f, "sum", "0")

	spec2 := &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID2, Input: &pb.ChaincodeInput{Args: args}}

	cccid2 := ccprovider.NewCCContext(chainID, "example05", "0", "", false, sProp, prop)

	_, err = deploy(ctxt, cccid2, spec2, nextBlockNumber)
	nextBlockNumber++
	ccID2 := spec2.ChaincodeId.Name
	if err != nil {
		theChaincodeSupport.Stop(ctxt, cccid1, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec1})
		theChaincodeSupport.Stop(ctxt, cccid2, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec2})
		t.Fail()
		t.Logf("Error initializing chaincode %s(%s)", ccID2, err)
		return
	}

	time.Sleep(time.Second)

	// Invoke second chaincode, which will inturn query the first chaincode
	f = "invoke"
	args = util.ToChaincodeArgs(f, ccID1, "sum")

	spec2 = &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID2, Input: &pb.ChaincodeInput{Args: args}}
	// Invoke chaincode
	var retVal []byte
	_, _, retVal, err = invoke(ctxt, chainID, spec2, nextBlockNumber, []byte("Alice"))
	nextBlockNumber++
	if err != nil {
		theChaincodeSupport.Stop(ctxt, cccid1, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec1})
		theChaincodeSupport.Stop(ctxt, cccid2, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec2})
		t.Fail()
		t.Logf("Error invoking <%s>: %s", ccID2, err)
		return
	}

	// Check the return value
	result, err := strconv.Atoi(string(retVal))
	if err != nil || result != 300 {
		theChaincodeSupport.Stop(ctxt, cccid1, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec1})
		theChaincodeSupport.Stop(ctxt, cccid2, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec2})
		t.Fail()
		t.Logf("Incorrect final state after transaction for <%s>: %s", ccID1, err)
		return
	}

	// Query second chaincode, which will inturn query the first chaincode
	f = "query"
	args = util.ToChaincodeArgs(f, ccID1, "sum")

	spec2 = &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID2, Input: &pb.ChaincodeInput{Args: args}}
	// Invoke chaincode
	_, _, retVal, err = invoke(ctxt, chainID, spec2, nextBlockNumber, []byte("Alice"))

	if err != nil {
		theChaincodeSupport.Stop(ctxt, cccid1, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec1})
		theChaincodeSupport.Stop(ctxt, cccid2, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec2})
		t.Fail()
		t.Logf("Error querying <%s>: %s", ccID2, err)
		return
	}

	// Check the return value
	result, err = strconv.Atoi(string(retVal))
	if err != nil || result != 300 {
		theChaincodeSupport.Stop(ctxt, cccid1, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec1})
		theChaincodeSupport.Stop(ctxt, cccid2, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec2})
		t.Fail()
		t.Logf("Incorrect final value after query for <%s>: %s", ccID1, err)
		return
	}

	theChaincodeSupport.Stop(ctxt, cccid1, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec1})
	theChaincodeSupport.Stop(ctxt, cccid2, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec2})
}

// test that invoking a security-sensitive system chaincode fails
func TestChaincodeInvokesForbiddenSystemChaincode(t *testing.T) {
	chainID := util.GetTestChainID()

	lis, err := initPeer(chainID)
	if err != nil {
		t.Fail()
		t.Logf("Error creating peer: %s", err)
	}

	defer finitPeer(lis, chainID)

	var ctxt = context.Background()

	var nextBlockNumber uint64

	// Deploy second chaincode
	url := "github.com/hyperledger/fabric/examples/chaincode/go/passthru"

	cID := &pb.ChaincodeID{Name: "pthru", Path: url, Version: "0"}
	f := "init"
	args := util.ToChaincodeArgs(f)

	spec := &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID, Input: &pb.ChaincodeInput{Args: args}}

	cccid := ccprovider.NewCCContext(chainID, "pthru", "0", "", false, nil, nil)

	_, err = deploy(ctxt, cccid, spec, nextBlockNumber)
	nextBlockNumber++
	ccID := spec.ChaincodeId.Name
	if err != nil {
		t.Fail()
		t.Logf("Error initializing chaincode %s(%s)", ccID, err)
		theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec})
		return
	}

	time.Sleep(time.Second)

	// send an invoke to pass thru to invoke "escc" system chaincode
	// this should fail
	args = util.ToChaincodeArgs("escc/"+chainID, "getid", chainID, "pthru")

	spec = &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID, Input: &pb.ChaincodeInput{Args: args}}
	// Invoke chaincode
	_, _, _, err = invoke(ctxt, chainID, spec, nextBlockNumber, nil)
	if err == nil {
		theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec})
		t.Logf("invoking <%s> should have failed", ccID)
		t.Fail()
		return
	}
}

// Test the execution of a chaincode that invokes system chaincode
// uses the "pthru" chaincode to query "lscc" for the "pthru" chaincode
func TestChaincodeInvokesSystemChaincode(t *testing.T) {
	chainID := util.GetTestChainID()

	lis, err := initPeer(chainID)
	if err != nil {
		t.Fail()
		t.Logf("Error creating peer: %s", err)
	}

	defer finitPeer(lis, chainID)

	var ctxt = context.Background()

	var nextBlockNumber uint64

	// Deploy second chaincode
	url := "github.com/hyperledger/fabric/examples/chaincode/go/passthru"

	cID := &pb.ChaincodeID{Name: "pthru", Path: url, Version: "0"}
	f := "init"
	args := util.ToChaincodeArgs(f)

	spec := &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID, Input: &pb.ChaincodeInput{Args: args}}

	cccid := ccprovider.NewCCContext(chainID, "pthru", "0", "", false, nil, nil)

	_, err = deploy(ctxt, cccid, spec, nextBlockNumber)
	nextBlockNumber++
	ccID := spec.ChaincodeId.Name
	if err != nil {
		t.Fail()
		t.Logf("Error initializing chaincode %s(%s)", ccID, err)
		theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec})
		return
	}

	time.Sleep(time.Second)

	//send an invoke to pass thru to query "lscc" system chaincode on chainID to get
	//information about "pthru"
	args = util.ToChaincodeArgs("lscc/"+chainID, "getid", chainID, "pthru")

	spec = &pb.ChaincodeSpec{Type: 1, ChaincodeId: cID, Input: &pb.ChaincodeInput{Args: args}}
	// Invoke chaincode
	_, _, retval, err := invoke(ctxt, chainID, spec, nextBlockNumber, nil)

	if err != nil {
		t.Fail()
		t.Logf("Error invoking <%s>: %s", ccID, err)
		theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec})
		return
	}

	if string(retval) != "pthru" {
		t.Fail()
		t.Logf("Expected to get back \"pthru\" from lscc but got back %s", string(retval))
		theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec})
		return
	}

	theChaincodeSupport.Stop(ctxt, cccid, &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec})
}

func TestChaincodeInitializeInitError(t *testing.T) {
	testCases := []struct {
		name          string
		chaincodeType pb.ChaincodeSpec_Type
		chaincodePath string
		args          []string
	}{
		{"NotSuccessResponse", pb.ChaincodeSpec_GOLANG, chaincodeExample02GolangPath, []string{"init", "not", "enough", "args"}},
		{"NotSuccessResponse", pb.ChaincodeSpec_JAVA, chaincodeExample02JavaPath, []string{"init", "not", "enough", "args"}},
		{"RuntimeException", pb.ChaincodeSpec_JAVA, chaincodeExample06JavaPath, []string{"runtimeException"}},
	}

	channelID := util.GetTestChainID()

	for _, tc := range testCases {
		t.Run(tc.name+"_"+tc.chaincodeType.String(), func(t *testing.T) {

			if tc.chaincodeType == pb.ChaincodeSpec_JAVA && runtime.GOARCH != "amd64" {
				t.Skip("No Java chaincode support yet on non-x86_64.")
			}

			// initialize peer
			if listener, err := initPeer(channelID); err != nil {
				t.Errorf("Error creating peer: %s", err)
			} else {
				defer finitPeer(listener, channelID)
			}

			var nextBlockNumber uint64

			// the chaincode to install and instanciate
			chaincodeName := generateChaincodeName(tc.chaincodeType)
			chaincodePath := tc.chaincodePath
			chaincodeVersion := "1.0.0.0"
			chaincodeType := tc.chaincodeType
			chaincodeDeployArgs := util.ArrayToChaincodeArgs(tc.args)

			// new chaincode context for passing around parameters
			chaincodeCtx := ccprovider.NewCCContext(channelID, chaincodeName, chaincodeVersion, "", false, nil, nil)

			// attempt to deploy chaincode
			_, err := deployChaincode(context.Background(), chaincodeCtx, chaincodeType, chaincodePath, chaincodeDeployArgs, nextBlockNumber)

			// deploy should of failed
			if err == nil {
				t.Fatal("Deployment should have failed.")
			}
			t.Log(err)

		})
	}
}

func TestMain(m *testing.M) {
	var err error

	// setup the MSP manager so that we can sign/verify
	mspMgrConfigDir := "../../msp/sampleconfig/"
	msptesttools.LoadMSPSetupForTesting(mspMgrConfigDir)
	signer, err = mspmgmt.GetLocalMSP().GetDefaultSigningIdentity()
	if err != nil {
		os.Exit(-1)
		fmt.Print("Could not initialize msp/signer")
		return
	}

	SetupTestConfig()
	os.Exit(m.Run())
}

func deployChaincode(ctx context.Context, chaincodeCtx *ccprovider.CCContext, chaincodeType pb.ChaincodeSpec_Type, path string, args [][]byte, nextBlockNumber uint64) ([]byte, error) {

	chaincodeSpec := &pb.ChaincodeSpec{
		ChaincodeId: &pb.ChaincodeID{
			Name:    chaincodeCtx.Name,
			Version: chaincodeCtx.Version,
			Path:    path,
		},
		Type: chaincodeType,
		Input: &pb.ChaincodeInput{
			Args: args,
		},
	}

	result, err := deploy(ctx, chaincodeCtx, chaincodeSpec, nextBlockNumber)
	if err != nil {
		return nil, fmt.Errorf("Error deploying <%s:%s>: %s", chaincodeSpec.ChaincodeId.Name, chaincodeSpec.ChaincodeId.Version, err)
	}
	return result, nil
}

var signer msp.SigningIdentity

var rng *rand.Rand = rand.New(rand.NewSource(time.Now().UnixNano()))

func generateChaincodeName(chaincodeType pb.ChaincodeSpec_Type) string {
	prefix := "cc_"
	switch chaincodeType {
	case pb.ChaincodeSpec_GOLANG:
		prefix = "cc_go_"
	case pb.ChaincodeSpec_JAVA:
		prefix = "cc_java_"
	case pb.ChaincodeSpec_NODE:
		prefix = "cc_js_"
	}
	return fmt.Sprintf("%s%06d", prefix, rng.Intn(999999))
}

type CreatorPolicy struct {
	Creators [][]byte
}

// Evaluate takes a set of SignedData and evaluates whether this set of signatures satisfies the policy
func (c *CreatorPolicy) Evaluate(signatureSet []*common.SignedData) error {
	for _, value := range c.Creators {
		if bytes.Compare(signatureSet[0].Identity, value) == 0 {
			return nil
		}
	}
	return fmt.Errorf("Creator not recognized [%s]", string(signatureSet[0].Identity))
}

type mockPolicyCheckerFactory struct{}

func (f *mockPolicyCheckerFactory) NewPolicyChecker() policy.PolicyChecker {
	return policy.NewPolicyChecker(
		peer.NewChannelPolicyManagerGetter(),
		&policy.MockIdentityDeserializer{[]byte("Admin"), []byte("msg1")},
		&policy.MockMSPPrincipalGetter{Principal: []byte("Admin")},
	)
}
