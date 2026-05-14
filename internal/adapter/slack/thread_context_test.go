package slack

import (
	"testing"
	"time"
)

func TestSlackMsgDedup_ShouldDeliver_SecondCallReportsAge(t *testing.T) {
	d := newSlackMsgDedup(512)
	ok, age := d.shouldDeliver("C123:111.222")
	if !ok || age != 0 {
		t.Fatalf("first deliver: ok=%v age=%v want ok=true age=0", ok, age)
	}
	time.Sleep(15 * time.Millisecond)
	ok2, age2 := d.shouldDeliver("C123:111.222")
	if ok2 {
		t.Fatal("second deliver should be rejected")
	}
	if age2 < 10*time.Millisecond {
		t.Fatalf("sinceFirst too small: %v", age2)
	}
}

func TestSlackMsgDedup_ShouldDeliver_DifferentKeys(t *testing.T) {
	d := newSlackMsgDedup(512)
	if ok, _ := d.shouldDeliver("a"); !ok {
		t.Fatal("a")
	}
	if ok, _ := d.shouldDeliver("b"); !ok {
		t.Fatal("b")
	}
}
