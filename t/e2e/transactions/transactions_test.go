package transactions

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/keystore"
	gethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/log"
	"github.com/status-im/status-go/geth/account"
	"github.com/status-im/status-go/geth/params"
	"github.com/status-im/status-go/geth/signal"
	"github.com/status-im/status-go/geth/transactions"
	"github.com/status-im/status-go/sign"
	e2e "github.com/status-im/status-go/t/e2e"
	. "github.com/status-im/status-go/t/utils"
	"github.com/stretchr/testify/suite"
)

type initFunc func([]byte, *transactions.SendTxArgs)

func TestTransactionsTestSuite(t *testing.T) {
	suite.Run(t, new(TransactionsTestSuite))
}

type TransactionsTestSuite struct {
	e2e.BackendTestSuite
}

func (s *TransactionsTestSuite) TestCallRPCSendTransaction() {
	s.StartTestBackend()
	defer s.StopTestBackend()

	EnsureNodeSync(s.Backend.StatusNode().EnsureSync)

	err := s.Backend.SelectAccount(TestConfig.Account1.Address, TestConfig.Account1.Password)
	s.NoError(err)

	transactionCompleted := make(chan struct{})

	var txHash gethcommon.Hash
	signal.SetDefaultNodeNotificationHandler(func(rawSignal string) {
		var sg signal.Envelope
		err := json.Unmarshal([]byte(rawSignal), &sg)
		s.NoError(err)

		if sg.Type == sign.EventTransactionQueued {
			event := sg.Event.(map[string]interface{})
			txID := event["id"].(string)
			txHash, err = s.Backend.CompleteTransaction(string(txID), TestConfig.Account1.Password)
			s.NoError(err, "cannot complete queued transaction %s", txID)
			close(transactionCompleted)
		}
	})

	result := s.Backend.CallRPC(`{
		"jsonrpc": "2.0",
		"id": 1,
		"method": "eth_sendTransaction",
		"params": [{
			"from": "` + TestConfig.Account1.Address + `",
			"to": "0xd46e8dd67c5d32be8058bb8eb970870f07244567",
			"value": "0x9184e72a"
		}]
	}`)
	s.NotContains(result, "error")

	select {
	case <-transactionCompleted:
	case <-time.After(time.Minute):
		s.FailNow("sending transaction timed out")
	}

	s.Equal(`{"jsonrpc":"2.0","id":1,"result":"`+txHash.String()+`"}`, result)
}

func (s *TransactionsTestSuite) TestCallRPCSendTransactionUpstream() {
	if GetNetworkID() == params.StatusChainNetworkID {
		s.T().Skip()
	}

	addr, err := GetRemoteURL()
	s.NoError(err)
	s.StartTestBackend(e2e.WithUpstream(addr))
	defer s.StopTestBackend()

	err = s.Backend.SelectAccount(TestConfig.Account2.Address, TestConfig.Account2.Password)
	s.NoError(err)

	transactionCompleted := make(chan struct{})

	var txHash gethcommon.Hash
	signal.SetDefaultNodeNotificationHandler(func(rawSignal string) {
		var signalEnvelope signal.Envelope
		err := json.Unmarshal([]byte(rawSignal), &signalEnvelope)
		s.NoError(err)

		if signalEnvelope.Type == sign.EventTransactionQueued {
			event := signalEnvelope.Event.(map[string]interface{})
			txID := event["id"].(string)

			// Complete with a wrong passphrase.
			txHash, err = s.Backend.CompleteTransaction(string(txID), "some-invalid-passphrase")
			s.EqualError(err, keystore.ErrDecrypt.Error(), "should return an error as the passphrase was invalid")

			// Complete with a correct passphrase.
			txHash, err = s.Backend.CompleteTransaction(string(txID), TestConfig.Account2.Password)
			s.NoError(err, "cannot complete queued transaction %s", txID)

			close(transactionCompleted)
		}
	})

	result := s.Backend.CallRPC(`{
		"jsonrpc": "2.0",
		"id": 1,
		"method": "eth_sendTransaction",
		"params": [{
			"from": "` + TestConfig.Account2.Address + `",
			"to": "` + TestConfig.Account1.Address + `",
			"value": "0x9184e72a"
		}]
	}`)
	s.NotContains(result, "error")

	select {
	case <-transactionCompleted:
	case <-time.After(time.Minute):
		s.FailNow("sending transaction timed out")
	}

	s.Equal(`{"jsonrpc":"2.0","id":1,"result":"`+txHash.String()+`"}`, result)
}

// TestSendContractCompat tries to send transaction using the legacy "Data"
// field, which is supported for backward compatibility reasons.
func (s *TransactionsTestSuite) TestSendContractTxCompat() {
	initFunc := func(byteCode []byte, args *transactions.SendTxArgs) {
		args.Data = (hexutil.Bytes)(byteCode)
	}
	s.testSendContractTx(initFunc, nil, "")
}

// TestSendContractCompat tries to send transaction using both the legacy
// "Data" and "Input" fields. Also makes sure that the error is returned if
// they have different values.
func (s *TransactionsTestSuite) TestSendContractTxCollision() {
	// Scenario 1: Both fields are filled and have the same value, expect success
	initFunc := func(byteCode []byte, args *transactions.SendTxArgs) {
		args.Input = (hexutil.Bytes)(byteCode)
		args.Data = (hexutil.Bytes)(byteCode)
	}
	s.testSendContractTx(initFunc, nil, "")

	s.NoError(s.Backend.AccountManager().Logout())

	// Scenario 2: Both fields are filled with different values, expect an error
	inverted := func(source []byte) []byte {
		inverse := make([]byte, len(source))
		copy(inverse, source)
		for i, b := range inverse {
			inverse[i] = b ^ 0xFF
		}
		return inverse
	}

	initFunc2 := func(byteCode []byte, args *transactions.SendTxArgs) {
		args.Input = (hexutil.Bytes)(byteCode)
		args.Data = (hexutil.Bytes)(inverted(byteCode))
	}
	s.testSendContractTx(initFunc2, transactions.ErrInvalidSendTxArgs, "expected error when invalid tx args are sent")
}

func (s *TransactionsTestSuite) TestSendContractTx() {
	initFunc := func(byteCode []byte, args *transactions.SendTxArgs) {
		args.Input = (hexutil.Bytes)(byteCode)
	}
	s.testSendContractTx(initFunc, nil, "")
}

func (s *TransactionsTestSuite) testSendContractTx(setInputAndDataValue initFunc, expectedError error, expectedErrorDescription string) {
	s.StartTestBackend()
	defer s.StopTestBackend()

	EnsureNodeSync(s.Backend.StatusNode().EnsureSync)

	sampleAddress, _, _, err := s.Backend.AccountManager().CreateAccount(TestConfig.Account1.Password)
	s.NoError(err)

	completeQueuedTransaction := make(chan struct{})

	// replace transaction notification handler
	var txHash gethcommon.Hash
	signal.SetDefaultNodeNotificationHandler(func(jsonEvent string) { // nolint :dupl
		var envelope signal.Envelope
		err = json.Unmarshal([]byte(jsonEvent), &envelope)
		s.NoError(err, fmt.Sprintf("cannot unmarshal JSON: %s", jsonEvent))

		if envelope.Type == sign.EventTransactionQueued {
			event := envelope.Event.(map[string]interface{})
			log.Info("transaction queued (will be completed shortly)", "id", event["id"].(string))

			// the first call will fail (we are not logged in, but trying to complete tx)
			log.Info("trying to complete with no user logged in")
			txHash, err = s.Backend.CompleteTransaction(
				string(event["id"].(string)),
				TestConfig.Account1.Password,
			)
			s.EqualError(
				err,
				account.ErrNoAccountSelected.Error(),
				fmt.Sprintf("expected error on queued transaction[%v] not thrown", event["id"]),
			)

			// the second call will also fail (we are logged in as different user)
			log.Info("trying to complete with invalid user")
			err = s.Backend.SelectAccount(sampleAddress, TestConfig.Account1.Password)
			s.NoError(err)
			txHash, err = s.Backend.CompleteTransaction(
				string(event["id"].(string)),
				TestConfig.Account1.Password,
			)
			s.EqualError(
				err,
				sign.ErrInvalidCompleteTxSender.Error(),
				fmt.Sprintf("expected error on queued transaction[%v] not thrown", event["id"]),
			)

			// the third call will work as expected (as we are logged in with correct credentials)
			log.Info("trying to complete with correct user, this should succeed")
			s.NoError(s.Backend.SelectAccount(TestConfig.Account1.Address, TestConfig.Account1.Password))
			txHash, err = s.Backend.CompleteTransaction(
				string(event["id"].(string)),
				TestConfig.Account1.Password,
			)
			if expectedError != nil {
				s.Equal(expectedError, err)
			} else {
				s.NoError(err, fmt.Sprintf("cannot complete queued transaction[%v]", event["id"]))
			}

			log.Info("contract transaction complete", "URL", "https://ropsten.etherscan.io/tx/"+txHash.Hex())
			close(completeQueuedTransaction)
			return
		}
	})

	// this call blocks, up until Complete Transaction is called
	byteCode, err := hexutil.Decode(`0x6060604052341561000c57fe5b5b60a58061001b6000396000f30060606040526000357c0100000000000000000000000000000000000000000000000000000000900463ffffffff1680636ffa1caa14603a575bfe5b3415604157fe5b60556004808035906020019091905050606b565b6040518082815260200191505060405180910390f35b60008160020290505b9190505600a165627a7a72305820ccdadd737e4ac7039963b54cee5e5afb25fa859a275252bdcf06f653155228210029`)
	s.NoError(err)

	gas := uint64(params.DefaultGas)
	args := transactions.SendTxArgs{
		From: account.FromAddress(TestConfig.Account1.Address),
		To:   nil, // marker, contract creation is expected
		//Value: (*hexutil.Big)(new(big.Int).Mul(big.NewInt(1), gethcommon.Ether)),
		Gas: (*hexutil.Uint64)(&gas),
	}

	setInputAndDataValue(byteCode, &args)
	txHashCheck, err := s.Backend.SendTransaction(context.TODO(), args)

	if expectedError != nil {
		s.Equal(expectedError, err, expectedErrorDescription)
		return
	}
	s.NoError(err, "cannot send transaction")

	select {
	case <-completeQueuedTransaction:
	case <-time.After(2 * time.Minute):
		s.FailNow("completing transaction timed out")
	}

	s.Equal(txHashCheck.Hex(), txHash.Hex(), "transaction hash returned from SendTransaction is invalid")
	s.False(reflect.DeepEqual(txHashCheck, gethcommon.Hash{}), "transaction was never queued or completed")
	s.Zero(s.PendingSignRequests().Count(), "tx queue must be empty at this point")
}

func (s *TransactionsTestSuite) TestSendEther() {
	s.StartTestBackend()
	defer s.StopTestBackend()

	EnsureNodeSync(s.Backend.StatusNode().EnsureSync)

	// create an account
	sampleAddress, _, _, err := s.Backend.AccountManager().CreateAccount(TestConfig.Account1.Password)
	s.NoError(err)

	completeQueuedTransaction := make(chan struct{})

	// replace transaction notification handler
	var txHash = gethcommon.Hash{}
	signal.SetDefaultNodeNotificationHandler(func(jsonEvent string) { // nolint: dupl
		var envelope signal.Envelope
		err = json.Unmarshal([]byte(jsonEvent), &envelope)
		s.NoError(err, fmt.Sprintf("cannot unmarshal JSON: %s", jsonEvent))

		if envelope.Type == sign.EventTransactionQueued {
			event := envelope.Event.(map[string]interface{})
			log.Info("transaction queued (will be completed shortly)", "id", event["id"].(string))

			// the first call will fail (we are not logged in, but trying to complete tx)
			log.Info("trying to complete with no user logged in")
			txHash, err = s.Backend.CompleteTransaction(
				string(event["id"].(string)),
				TestConfig.Account1.Password,
			)
			s.EqualError(
				err,
				account.ErrNoAccountSelected.Error(),
				fmt.Sprintf("expected error on queued transaction[%v] not thrown", event["id"]),
			)

			// the second call will also fail (we are logged in as different user)
			log.Info("trying to complete with invalid user")
			err = s.Backend.SelectAccount(sampleAddress, TestConfig.Account1.Password)
			s.NoError(err)
			txHash, err = s.Backend.CompleteTransaction(
				string(event["id"].(string)), TestConfig.Account1.Password)
			s.EqualError(
				err,
				sign.ErrInvalidCompleteTxSender.Error(),
				fmt.Sprintf("expected error on queued transaction[%v] not thrown", event["id"]),
			)

			// the third call will work as expected (as we are logged in with correct credentials)
			log.Info("trying to complete with correct user, this should succeed")
			s.NoError(s.Backend.SelectAccount(TestConfig.Account1.Address, TestConfig.Account1.Password))
			txHash, err = s.Backend.CompleteTransaction(
				string(event["id"].(string)),
				TestConfig.Account1.Password,
			)
			s.NoError(err, fmt.Sprintf("cannot complete queued transaction[%v]", event["id"]))

			close(completeQueuedTransaction)
			return
		}
	})

	// this call blocks, up until Complete Transaction is called
	txHashCheck, err := s.Backend.SendTransaction(context.TODO(), transactions.SendTxArgs{
		From:  account.FromAddress(TestConfig.Account1.Address),
		To:    account.ToAddress(TestConfig.Account2.Address),
		Value: (*hexutil.Big)(big.NewInt(1000000000000)),
	})
	s.NoError(err, "cannot send transaction")

	select {
	case <-completeQueuedTransaction:
	case <-time.After(2 * time.Minute):
		s.FailNow("completing transaction timed out")
	}

	s.Equal(txHashCheck.Hex(), txHash.Hex(), "transaction hash returned from SendTransaction is invalid")
	s.False(reflect.DeepEqual(txHashCheck, gethcommon.Hash{}), "transaction was never queued or completed")
	s.Zero(s.Backend.PendingSignRequests().Count(), "tx queue must be empty at this point")
}

func (s *TransactionsTestSuite) TestSendEtherTxUpstream() {
	if GetNetworkID() == params.StatusChainNetworkID {
		s.T().Skip()
	}

	addr, err := GetRemoteURL()
	s.NoError(err)
	s.StartTestBackend(e2e.WithUpstream(addr))
	defer s.StopTestBackend()

	err = s.Backend.SelectAccount(TestConfig.Account1.Address, TestConfig.Account1.Password)
	s.NoError(err)

	completeQueuedTransaction := make(chan struct{})

	// replace transaction notification handler
	var txHash = gethcommon.Hash{}
	signal.SetDefaultNodeNotificationHandler(func(jsonEvent string) { // nolint: dupl
		var envelope signal.Envelope
		err = json.Unmarshal([]byte(jsonEvent), &envelope)
		s.NoError(err, "cannot unmarshal JSON: %s", jsonEvent)

		if envelope.Type == sign.EventTransactionQueued {
			event := envelope.Event.(map[string]interface{})
			log.Info("transaction queued (will be completed shortly)", "id", event["id"].(string))

			txHash, err = s.Backend.CompleteTransaction(
				string(event["id"].(string)),
				TestConfig.Account1.Password,
			)
			s.NoError(err, "cannot complete queued transaction[%v]", event["id"])

			log.Info("contract transaction complete", "URL", "https://ropsten.etherscan.io/tx/"+txHash.Hex())
			close(completeQueuedTransaction)
		}
	})

	// This call blocks, up until Complete Transaction is called.
	// Explicitly not setting Gas to get it estimated.
	txHashCheck, err := s.Backend.SendTransaction(context.TODO(), transactions.SendTxArgs{
		From:     account.FromAddress(TestConfig.Account1.Address),
		To:       account.ToAddress(TestConfig.Account2.Address),
		GasPrice: (*hexutil.Big)(big.NewInt(28000000000)),
		Value:    (*hexutil.Big)(big.NewInt(1000000000000)),
	})
	s.NoError(err, "cannot send transaction")

	select {
	case <-completeQueuedTransaction:
	case <-time.After(1 * time.Minute):
		s.FailNow("completing transaction timed out")
	}

	s.Equal(txHash.Hex(), txHashCheck.Hex(), "transaction hash returned from SendTransaction is invalid")
	s.Zero(s.Backend.PendingSignRequests().Count(), "tx queue must be empty at this point")
}

func (s *TransactionsTestSuite) TestDoubleCompleteQueuedTransactions() {
	s.StartTestBackend()
	defer s.StopTestBackend()

	EnsureNodeSync(s.Backend.StatusNode().EnsureSync)

	// log into account from which transactions will be sent
	s.NoError(s.Backend.SelectAccount(TestConfig.Account1.Address, TestConfig.Account1.Password))

	completeQueuedTransaction := make(chan struct{})

	// replace transaction notification handler
	txFailedEventCalled := false
	txHash := gethcommon.Hash{}
	signal.SetDefaultNodeNotificationHandler(func(jsonEvent string) {
		var envelope signal.Envelope
		err := json.Unmarshal([]byte(jsonEvent), &envelope)
		s.NoError(err, fmt.Sprintf("cannot unmarshal JSON: %s", jsonEvent))

		if envelope.Type == sign.EventTransactionQueued {
			event := envelope.Event.(map[string]interface{})
			txID := string(event["id"].(string))
			log.Info("transaction queued (will be failed and completed on the second call)", "id", txID)

			// try with wrong password
			// make sure that tx is NOT removed from the queue (by re-trying with the correct password)
			_, err = s.Backend.CompleteTransaction(txID, TestConfig.Account1.Password+"wrong")
			s.EqualError(err, keystore.ErrDecrypt.Error())

			s.Equal(1, s.PendingSignRequests().Count(), "txqueue cannot be empty, as tx has failed")

			// now try to complete transaction, but with the correct password
			txHash, err = s.Backend.CompleteTransaction(txID, TestConfig.Account1.Password)
			s.NoError(err)

			log.Info("transaction complete", "URL", "https://rinkeby.etherscan.io/tx/"+txHash.Hex())
			close(completeQueuedTransaction)
		}

		if envelope.Type == sign.EventTransactionFailed {
			event := envelope.Event.(map[string]interface{})
			log.Info("transaction return event received", "id", event["id"].(string))

			receivedErrMessage := event["error_message"].(string)
			expectedErrMessage := "could not decrypt key with given passphrase"
			s.Equal(receivedErrMessage, expectedErrMessage)

			receivedErrCode := event["error_code"].(string)
			s.Equal("2", receivedErrCode)

			txFailedEventCalled = true
		}
	})

	// this call blocks, and should return on *second* attempt to CompleteTransaction (w/ the correct password)
	txHashCheck, err := s.Backend.SendTransaction(context.TODO(), transactions.SendTxArgs{
		From:  account.FromAddress(TestConfig.Account1.Address),
		To:    account.ToAddress(TestConfig.Account2.Address),
		Value: (*hexutil.Big)(big.NewInt(1000000000000)),
	})
	s.NoError(err, "cannot send transaction")

	select {
	case <-completeQueuedTransaction:
	case <-time.After(time.Minute):
		s.FailNow("test timed out")
	}

	s.Equal(txHashCheck.Hex(), txHash.Hex(), "transaction hash returned from SendTransaction is invalid")
	s.False(reflect.DeepEqual(txHashCheck, gethcommon.Hash{}), "transaction was never queued or completed")
	s.Zero(s.Backend.PendingSignRequests().Count(), "tx queue must be empty at this point")
	s.True(txFailedEventCalled, "expected tx failure signal is not received")
}

func (s *TransactionsTestSuite) TestDiscardQueuedTransaction() {
	s.StartTestBackend()
	defer s.StopTestBackend()

	EnsureNodeSync(s.Backend.StatusNode().EnsureSync)

	// log into account from which transactions will be sent
	s.NoError(s.Backend.SelectAccount(TestConfig.Account1.Address, TestConfig.Account1.Password))

	completeQueuedTransaction := make(chan struct{})

	// replace transaction notification handler
	txFailedEventCalled := false
	signal.SetDefaultNodeNotificationHandler(func(jsonEvent string) {
		var envelope signal.Envelope
		err := json.Unmarshal([]byte(jsonEvent), &envelope)
		s.NoError(err, fmt.Sprintf("cannot unmarshal JSON: %s", jsonEvent))

		if envelope.Type == sign.EventTransactionQueued {
			event := envelope.Event.(map[string]interface{})
			txID := string(event["id"].(string))
			log.Info("transaction queued (will be discarded soon)", "id", txID)

			s.True(s.Backend.PendingSignRequests().Has(txID), "txqueue should still have test tx")

			// discard
			err := s.Backend.DiscardTransaction(txID)
			s.NoError(err, "cannot discard tx")

			// try completing discarded transaction
			_, err = s.Backend.CompleteTransaction(txID, TestConfig.Account1.Password)
			s.EqualError(err, "transaction hash not found", "expects tx not found, but call to CompleteTransaction succeeded")

			time.Sleep(1 * time.Second) // make sure that tx complete signal propagates
			s.False(s.Backend.PendingSignRequests().Has(txID),
				fmt.Sprintf("txqueue should not have test tx at this point (it should be discarded): %s", txID))

			close(completeQueuedTransaction)
		}

		if envelope.Type == sign.EventTransactionFailed {
			event := envelope.Event.(map[string]interface{})
			log.Info("transaction return event received", "id", event["id"].(string))

			receivedErrMessage := event["error_message"].(string)
			expectedErrMessage := sign.ErrSignReqDiscarded.Error()
			s.Equal(receivedErrMessage, expectedErrMessage)

			receivedErrCode := event["error_code"].(string)
			s.Equal("4", receivedErrCode)

			txFailedEventCalled = true
		}
	})

	// this call blocks, and should return when DiscardQueuedTransaction() is called
	txHashCheck, err := s.Backend.SendTransaction(context.TODO(), transactions.SendTxArgs{
		From:  account.FromAddress(TestConfig.Account1.Address),
		To:    account.ToAddress(TestConfig.Account2.Address),
		Value: (*hexutil.Big)(big.NewInt(1000000000000)),
	})
	s.EqualError(err, sign.ErrSignReqDiscarded.Error(), "transaction is expected to be discarded")

	select {
	case <-completeQueuedTransaction:
	case <-time.After(10 * time.Second):
		s.FailNow("test timed out")
	}

	s.True(reflect.DeepEqual(txHashCheck, gethcommon.Hash{}), "transaction returned hash, while it shouldn't")
	s.Zero(s.Backend.PendingSignRequests().Count(), "tx queue must be empty at this point")
	s.True(txFailedEventCalled, "expected tx failure signal is not received")
}

func (s *TransactionsTestSuite) TestCompleteMultipleQueuedTransactions() {
	s.setupLocalNode()
	defer s.StopTestBackend()

	// log into account from which transactions will be sent
	err := s.Backend.SelectAccount(TestConfig.Account1.Address, TestConfig.Account1.Password)
	s.NoError(err)

	s.sendConcurrentTransactions(3)
}

func (s *TransactionsTestSuite) TestDiscardMultipleQueuedTransactions() {
	s.StartTestBackend()
	defer s.StopTestBackend()

	EnsureNodeSync(s.Backend.StatusNode().EnsureSync)

	// log into account from which transactions will be sent
	s.NoError(s.Backend.SelectAccount(TestConfig.Account1.Address, TestConfig.Account1.Password))

	testTxCount := 3
	txIDs := make(chan string, testTxCount)
	allTestTxDiscarded := make(chan struct{})

	// replace transaction notification handler
	txFailedEventCallCount := 0
	signal.SetDefaultNodeNotificationHandler(func(jsonEvent string) {
		var envelope signal.Envelope
		err := json.Unmarshal([]byte(jsonEvent), &envelope)
		s.NoError(err)
		if envelope.Type == sign.EventTransactionQueued {
			event := envelope.Event.(map[string]interface{})
			txID := string(event["id"].(string))
			log.Info("transaction queued (will be discarded soon)", "id", txID)

			s.True(s.Backend.PendingSignRequests().Has(txID),
				"txqueue should still have test tx")
			txIDs <- txID
		}

		if envelope.Type == sign.EventTransactionFailed {
			event := envelope.Event.(map[string]interface{})
			log.Info("transaction return event received", "id", event["id"].(string))

			receivedErrMessage := event["error_message"].(string)
			expectedErrMessage := sign.ErrSignReqDiscarded.Error()
			s.Equal(receivedErrMessage, expectedErrMessage)

			receivedErrCode := event["error_code"].(string)
			s.Equal("4", receivedErrCode)

			txFailedEventCallCount++
			if txFailedEventCallCount == testTxCount {
				close(allTestTxDiscarded)
			}
		}
	})

	require := s.Require()

	//  this call blocks, and should return when DiscardQueuedTransaction() for a given tx id is called
	sendTx := func() {
		txHashCheck, err := s.Backend.SendTransaction(context.TODO(), transactions.SendTxArgs{
			From:  account.FromAddress(TestConfig.Account1.Address),
			To:    account.ToAddress(TestConfig.Account2.Address),
			Value: (*hexutil.Big)(big.NewInt(1000000000000)),
		})
		require.EqualError(err, sign.ErrSignReqDiscarded.Error())
		require.Equal(gethcommon.Hash{}, txHashCheck, "transaction returned hash, while it shouldn't")
	}

	signRequests := s.Backend.PendingSignRequests()

	// wait for transactions, and discard immediately
	discardTxs := func(txIDs []string) {
		txIDs = append(txIDs, "invalid-tx-id")

		// discard
		discardResults := s.Backend.DiscardTransactions(txIDs)
		require.Len(discardResults, 1, "cannot discard txs: %v", discardResults)
		require.Error(discardResults["invalid-tx-id"], "transaction hash not found", "cannot discard txs: %v", discardResults)

		// try completing discarded transaction
		completeResults := s.Backend.CompleteTransactions(txIDs, TestConfig.Account1.Password)
		require.Len(completeResults, testTxCount+1, "unexpected number of errors (call to CompleteTransaction should not succeed)")

		for _, txResult := range completeResults {
			require.Error(txResult.Error, "transaction hash not found", "invalid error for %s", txResult.Hash.Hex())
			require.Equal(gethcommon.Hash{}, txResult.Hash, "invalid hash (expected zero): %s", txResult.Hash.Hex())
		}

		time.Sleep(1 * time.Second) // make sure that tx complete signal propagates

		for _, txID := range txIDs {
			require.False(
				signRequests.Has(txID),
				"txqueue should not have test tx at this point (it should be discarded): %s",
				txID,
			)
		}
	}
	go func() {
		ids := make([]string, testTxCount)
		for i := 0; i < testTxCount; i++ {
			ids[i] = <-txIDs
		}

		discardTxs(ids)
	}()

	// send multiple transactions
	for i := 0; i < testTxCount; i++ {
		go sendTx()
	}

	select {
	case <-allTestTxDiscarded:
	case <-time.After(1 * time.Minute):
		s.FailNow("test timed out")
	}
	time.Sleep(5 * time.Second)

	s.Zero(s.Backend.PendingSignRequests().Count(), "tx queue must be empty at this point")
}

func (s *TransactionsTestSuite) TestNonExistentQueuedTransactions() {
	s.StartTestBackend()
	defer s.StopTestBackend()

	// log into account from which transactions will be sent
	s.NoError(s.Backend.SelectAccount(TestConfig.Account1.Address, TestConfig.Account1.Password))

	// replace transaction notification handler
	signal.SetDefaultNodeNotificationHandler(func(string) {})

	// try completing non-existing transaction
	_, err := s.Backend.CompleteTransaction("some-bad-transaction-id", TestConfig.Account1.Password)
	s.Error(err, "error expected and not received")
	s.EqualError(err, sign.ErrSignReqNotFound.Error())
}

func (s *TransactionsTestSuite) TestCompleteMultipleQueuedTransactionsUpstream() {
	s.setupUpstreamNode()
	defer s.StopTestBackend()

	// log into account from which transactions will be sent
	err := s.Backend.SelectAccount(TestConfig.Account1.Address, TestConfig.Account1.Password)
	s.NoError(err)

	s.sendConcurrentTransactions(30)
}

func (s *TransactionsTestSuite) setupLocalNode() {
	s.StartTestBackend()

	EnsureNodeSync(s.Backend.StatusNode().EnsureSync)
}

func (s *TransactionsTestSuite) setupUpstreamNode() {
	if GetNetworkID() == params.StatusChainNetworkID {
		s.T().Skip()
	}

	addr, err := GetRemoteURL()
	s.NoError(err)
	s.StartTestBackend(e2e.WithUpstream(addr))
}

func (s *TransactionsTestSuite) sendConcurrentTransactions(testTxCount int) {
	txIDs := make(chan string, testTxCount)
	allTestTxCompleted := make(chan struct{})

	require := s.Require()

	// replace transaction notification handler
	signal.SetDefaultNodeNotificationHandler(func(jsonEvent string) {
		var envelope signal.Envelope
		err := json.Unmarshal([]byte(jsonEvent), &envelope)
		require.NoError(err, fmt.Sprintf("cannot unmarshal JSON: %s", jsonEvent))

		if envelope.Type == sign.EventTransactionQueued {
			event := envelope.Event.(map[string]interface{})
			txID := string(event["id"].(string))
			log.Info("transaction queued (will be completed in a single call, once aggregated)", "id", txID)

			txIDs <- txID
		}
	})

	//  this call blocks, and should return when DiscardQueuedTransaction() for a given tx id is called
	sendTx := func() {
		txHashCheck, err := s.Backend.SendTransaction(context.TODO(), transactions.SendTxArgs{
			From:  account.FromAddress(TestConfig.Account1.Address),
			To:    account.ToAddress(TestConfig.Account2.Address),
			Value: (*hexutil.Big)(big.NewInt(1000000000000)),
		})
		require.NoError(err, "cannot send transaction")
		require.NotEqual(gethcommon.Hash{}, txHashCheck, "transaction returned empty hash")
	}

	// wait for transactions, and complete them in a single call
	completeTxs := func(txIDs []string) {
		txIDs = append(txIDs, "invalid-tx-id")
		results := s.Backend.CompleteTransactions(txIDs, TestConfig.Account1.Password)
		s.Len(results, testTxCount+1)
		s.EqualError(results["invalid-tx-id"].Error, "transaction hash not found")

		for txID, txResult := range results {
			s.False(
				txResult.Error != nil && txID != "invalid-tx-id",
				"invalid error for %s", txID,
			)
			s.False(
				txResult.Hash == (gethcommon.Hash{}) && txID != "invalid-tx-id",
				"invalid hash (expected non empty hash): %s", txID,
			)
			log.Info("transaction complete", "URL", "https://ropsten.etherscan.io/tx/"+txResult.Hash.Hex())
		}

		time.Sleep(1 * time.Second) // make sure that tx complete signal propagates

		for _, txID := range txIDs {
			s.False(
				s.Backend.PendingSignRequests().Has(txID),
				"txqueue should not have test tx at this point (it should be completed)",
			)
		}
	}
	go func() {
		ids := make([]string, testTxCount)
		for i := 0; i < testTxCount; i++ {
			ids[i] = <-txIDs
		}

		completeTxs(ids)
		close(allTestTxCompleted)
	}()

	// send multiple transactions
	for i := 0; i < testTxCount; i++ {
		go sendTx()
	}

	select {
	case <-allTestTxCompleted:
	case <-time.After(60 * time.Second):
		s.FailNow("test timed out")
	}

	s.Zero(s.PendingSignRequests().Count(), "queue should be empty")
}
