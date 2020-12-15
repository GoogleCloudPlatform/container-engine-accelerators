package pci

// PciDetailsGetter is used to map a device id (such as nvidia0) to a PCI bus id.
type PciDetailsGetter interface {
	GetPciBusID(deviceID string) (string, error)
}
