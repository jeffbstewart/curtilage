package policy

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Describe is the one sentence for an event, built from its structure
// so the server's interim notifier and the house page say the same
// thing the app will (the app renders from the same fields; no free
// text crosses a relay).  It gets better as the event does:
//
//	Person on the porch
//	Person and a dog on the porch
//	2 persons and a dog started on the porch, moved to the yard
//	2 persons and a dog started on the porch, moved to the yard, then the driveway (1m25s)
func Describe(e Event) string {
	if e.Kind != KindActivity {
		s := e.Label
		if e.SubLabel != "" {
			s += " (" + e.SubLabel + ")"
		}
		if len(e.Zones) > 0 {
			s += " in " + strings.Join(e.Zones, ", ")
		}
		return capitalize(s)
	}
	s := subject(e.Objects)
	switch len(e.Path) {
	case 0:
		s += " seen"
	case 1:
		s += " " + at(e.Path[0])
	default:
		s += " started " + at(e.Path[0]) + ", moved to the " + place(e.Path[1])
		for _, z := range e.Path[2:] {
			s += ", then the " + place(z)
		}
	}
	if !e.Running() {
		s += " (" + e.EndedAt.Sub(e.StartedAt).Round(time.Second).String() + ")"
	}
	return capitalize(s)
}

// subject: "person", "person and a dog", "2 persons, a dog and a cat".
func subject(objects map[string]int) string {
	labels := make([]string, 0, len(objects))
	for l, n := range objects {
		if n > 0 {
			labels = append(labels, l)
		}
	}
	sort.Slice(labels, func(i, j int) bool { // person first, then alphabetical
		if (labels[i] == "person") != (labels[j] == "person") {
			return labels[i] == "person"
		}
		return labels[i] < labels[j]
	})
	parts := make([]string, 0, len(labels))
	for _, l := range labels {
		parts = append(parts, noun(l, objects[l]))
	}
	switch len(parts) {
	case 0:
		return "something"
	case 1:
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// noun: "person", "2 persons", "a dog", "3 dogs".
func noun(label string, n int) string {
	if n == 1 {
		if label == "person" {
			return "person"
		}
		return "a " + label
	}
	return fmt.Sprintf("%d %ss", n, label)
}

// at: "on the porch", "in the yard".
func at(zone string) string {
	p := place(zone)
	if strings.Contains(p, "porch") || strings.Contains(p, "deck") || strings.Contains(p, "step") {
		return "on the " + p
	}
	return "in the " + p
}

func place(zone string) string { return strings.ReplaceAll(zone, "_", " ") }

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
