package policy

import (
	"testing"
	"time"
)

func TestDescribeActivity(t *testing.T) {
	t0 := time.Date(2026, 8, 30, 16, 4, 49, 0, time.UTC)
	act := func(objects map[string]int, path []string, ended time.Duration) Event {
		e := Event{Kind: KindActivity, Objects: objects, Path: path, StartedAt: t0}
		if ended > 0 {
			e.EndedAt = t0.Add(ended)
		}
		return e
	}
	cases := []struct {
		e    Event
		want string
	}{
		{act(map[string]int{"person": 1}, []string{"porch"}, 0), "Person on the porch"},
		{act(map[string]int{"person": 1, "dog": 1}, []string{"porch"}, 0), "Person and a dog on the porch"},
		{act(map[string]int{"person": 2, "dog": 1}, []string{"porch", "yard"}, 0), "2 persons and a dog started on the porch, moved to the yard"},
		{act(map[string]int{"person": 2, "dog": 1}, []string{"porch", "yard", "driveway"}, 85*time.Second), "2 persons and a dog started on the porch, moved to the yard, then the driveway (1m25s)"},
		{act(map[string]int{"dog": 1}, []string{"yard"}, 0), "A dog in the yard"},
		{act(map[string]int{"dog": 2, "cat": 1, "person": 1}, []string{"side_parking"}, 0), "Person, a cat and 2 dogs in the side parking"},
		{act(map[string]int{"person": 1}, nil, 0), "Person seen"},
		{act(map[string]int{"person": 1}, []string{"driveway", "yard", "porch"}, 57*time.Second), "Person started in the driveway, moved to the yard, then the porch (57s)"},
		{Event{Kind: KindDetection, Label: "car", SubLabel: "blue minivan", Zones: []string{"side_parking"}}, "Car (blue minivan) in side_parking"},
		{Event{Kind: KindDetection, Label: "car"}, "Car"},
	}
	for _, c := range cases {
		if got := Describe(c.e); got != c.want {
			t.Errorf("Describe = %q, want %q", got, c.want)
		}
	}
}
