package pci

type PciDetailsGetter interface {
	GetPciBusId(deviceId string) (string, error)
}
