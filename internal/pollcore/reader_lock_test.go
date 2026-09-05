package pollcore

import (
	"testing"

	"mbgw/internal/transport"
)

func TestPhysicalIOLockKey_SharedCOMUsesSameLock(t *testing.T) {
	first := transport.Params{
		Kind: transport.KindRTUSerial,
		COM:  "COM101",
	}
	second := transport.Params{
		Kind: transport.KindRTUSerial,
		COM:  "com101",
	}

	keyA := PhysicalIOLockKey(first, "vkm-1")
	keyB := PhysicalIOLockKey(second, "vkm-2")

	if keyA == "" {
		t.Fatal("expected non-empty physical I/O lock key")
	}
	if keyA != keyB {
		t.Fatalf("devices on the same COM port must share one I/O lock: %q != %q", keyA, keyB)
	}

	muA := sharedIOMutex(keyA)
	muB := sharedIOMutex(keyB)
	if muA != muB {
		t.Fatal("devices on the same COM port received different physical I/O mutexes")
	}
}

func TestPhysicalIOLockKey_DifferentCOMUsesDifferentLock(t *testing.T) {
	keyA := PhysicalIOLockKey(transport.Params{Kind: transport.KindRTUSerial, COM: "COM101"}, "vkm-1")
	keyB := PhysicalIOLockKey(transport.Params{Kind: transport.KindRTUSerial, COM: "COM102"}, "vkm-2")

	if keyA == keyB {
		t.Fatalf("different COM ports must not share one I/O lock key: %q", keyA)
	}
}

func TestPhysicalIOLockKey_ModbusTCPRemainsPerDevice(t *testing.T) {
	params := transport.Params{
		Kind: transport.KindModbusTCP,
		Host: "10.48.228.160",
		Port: 502,
	}

	keyA := PhysicalIOLockKey(params, "vkm-1")
	keyB := PhysicalIOLockKey(params, "vkm-2")

	if keyA == keyB {
		t.Fatalf("native Modbus TCP devices must remain isolated per device: %q", keyA)
	}
}
