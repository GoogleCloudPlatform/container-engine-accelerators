package numa

// NewMockNumaNodeGetter returns a mock NumaNodeGetter for unit testing
func NewMockNumaNodeGetter(mockNumaNode int) NumaNodeGetter {
	return &mockNumaNodeGetter{mockNumaNode: mockNumaNode}
}

type mockNumaNodeGetter struct {
	mockNumaNode int
}

func (s *mockNumaNodeGetter) Get(deviceID string) (int, error) {
	return s.mockNumaNode, nil
}
