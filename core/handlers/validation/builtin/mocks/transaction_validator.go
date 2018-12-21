// Code generated by mockery v1.0.0. DO NOT EDIT.
package mocks

import errors "justledger/common/errors"
import mock "github.com/stretchr/testify/mock"

// TransactionValidator is an autogenerated mock type for the TransactionValidator type
type TransactionValidator struct {
	mock.Mock
}

// Validate provides a mock function with given fields: txData, policy
func (_m *TransactionValidator) Validate(txData []byte, policy []byte) errors.TxValidationError {
	ret := _m.Called(txData, policy)

	var r0 errors.TxValidationError
	if rf, ok := ret.Get(0).(func([]byte, []byte) errors.TxValidationError); ok {
		r0 = rf(txData, policy)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(errors.TxValidationError)
		}
	}

	return r0
}
