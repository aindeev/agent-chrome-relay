package device

import "testing"

func TestIDIsStableAndHidesTheUUID(t *testing.T) {
	a, b := Hash("12345678-ABCD-4000-8000-000000000001"), Hash("12345678-abcd-4000-8000-000000000001")
	if a != b || len(a) != 16 || a == Hash("12345678-ABCD-4000-8000-000000000002") {
		t.Fatal(a, b)
	}
	if ID() == "" || ID() != ID() {
		t.Fatal("this Mac's ID", ID())
	}
}
