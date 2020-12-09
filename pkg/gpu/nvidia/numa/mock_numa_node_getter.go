package numa

func NewMockNumaNodeGetter(mockNumaNode int) NumaNodeGetter {
	return &mockNumaNodeGetter{mockNumaNode: mockNumaNode}
}

type mockNumaNodeGetter struct {
	mockNumaNode int
}

func (s *mockNumaNodeGetter) Get(deviceId string) (int, error) {
	return s.mockNumaNode, nil
}
