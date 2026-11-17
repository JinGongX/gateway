package health

import "testing"

func TestDrainer(t *testing.T) {
	var d Drainer
	if d.Draining() {
		t.Error("零值不应处于下线状态")
	}
	d.Start()
	if !d.Draining() {
		t.Error("Start 之后应处于下线状态")
	}
}

func TestNilDrainerIsNeverDraining(t *testing.T) {
	var d *Drainer
	if d.Draining() {
		t.Error("nil Drainer 应恒为 false，调用方不必判空")
	}
}
