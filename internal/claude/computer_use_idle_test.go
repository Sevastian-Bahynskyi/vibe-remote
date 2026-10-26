package claude

import "testing"

func TestIdleComputerUseSkipsTerminalSerialization(t *testing.T) {
	approver := &computerUseApprover{request: func() string { return "" }}
	if approver.approve() {
		t.Fatal("idle approval requested fast polling")
	}
	pending := newComputerUseApprover(&execProcess{}, func() string { return "current-computer-call" })
	if !pending.approve() {
		t.Fatal("pending computer call did not request fast polling")
	}
}
