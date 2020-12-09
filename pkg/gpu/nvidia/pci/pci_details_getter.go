package pci

// PciDetailsGetter is used to map a device id (such as nvidia0) to a PCI bus id.
type PciDetailsGetter interface {
	GetPciBusId(deviceID string) (string, error)
}
