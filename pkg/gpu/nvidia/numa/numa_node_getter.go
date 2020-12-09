package numa

type NumaNodeGetter interface {
	Get(deviceId string) (int, error)
}
