package mqtt

import (
	"testing"

	curtilagev1 "github.com/jeffbstewart/curtilage/gen/curtilage/v1"
)

func TestQosEnum(t *testing.T) {
	cases := map[byte]curtilagev1.Qos{
		0: curtilagev1.Qos_QOS_AT_MOST_ONCE,
		1: curtilagev1.Qos_QOS_AT_LEAST_ONCE,
		2: curtilagev1.Qos_QOS_EXACTLY_ONCE,
		7: curtilagev1.Qos_QOS_UNSPECIFIED,
	}
	for in, want := range cases {
		if got := qosEnum(in); got != want {
			t.Errorf("qosEnum(%d) = %v, want %v", in, got, want)
		}
	}
}
