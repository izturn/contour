// Code generated by mockery v2.15.0. DO NOT EDIT.

package mocks

import mock "github.com/stretchr/testify/mock"

// NeedLeaderElectionNotification is an autogenerated mock type for the NeedLeaderElectionNotification type
type NeedLeaderElectionNotification struct {
	mock.Mock
}

// OnElectedLeader provides a mock function with given fields:
func (_m *NeedLeaderElectionNotification) OnElectedLeader() {
	_m.Called()
}

type mockConstructorTestingTNewNeedLeaderElectionNotification interface {
	mock.TestingT
	Cleanup(func())
}

// NewNeedLeaderElectionNotification creates a new instance of NeedLeaderElectionNotification. It also registers a testing interface on the mock and a cleanup function to assert the mocks expectations.
func NewNeedLeaderElectionNotification(t mockConstructorTestingTNewNeedLeaderElectionNotification) *NeedLeaderElectionNotification {
	mock := &NeedLeaderElectionNotification{}
	mock.Mock.Test(t)

	t.Cleanup(func() { mock.AssertExpectations(t) })

	return mock
}
