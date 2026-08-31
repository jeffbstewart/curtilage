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
	switch e.Kind {
	case KindSighting:
		// Rare enough that the sighting itself is the news; without a
		// zone the camera says where.
		s := "a " + e.Label + " sighted"
		if len(e.Zones) > 0 {
			s += " " + at(e.Zones[0])
		} else if e.Camera != "" {
			s += " (" + e.Camera + ")"
		}
		return capitalize(s)
	case KindState:
		// Label is the model, SubLabel the class; a transition is
		// instantaneous, an alarm has endured started..ended.
		s := strings.ReplaceAll(e.Label, "_", " ") + ": " + Humanize(e.Label, e.SubLabel)
		if d := e.EndedAt.Sub(e.StartedAt); d > 0 {
			s += " for " + d.Round(time.Minute).String()
		}
		return capitalize(s)
	case KindArrival, KindDeparture:
		zone := ""
		if len(e.Zones) > 0 {
			zone = " " + at(e.Zones[0])
		}
		who := "A " + e.Label
		if id := ident(e); id != "" {
			who += " (" + id + ")"
		}
		n := e.Objects[e.Label]
		if e.Kind == KindArrival {
			s := who + " arrived" + zone
			if n > 1 {
				s += fmt.Sprintf(" (%d present)", n)
			}
			return capitalize(s)
		}
		if len(e.Zones) > 0 {
			zone = " the " + place(e.Zones[0])
		}
		s := who + " left" + zone
		switch n {
		case 0:
			s += " (empty now)"
		case 1:
			s += " (1 remains)"
		default:
			s += fmt.Sprintf(" (%d remain)", n)
		}
		return capitalize(s)
	}
	if e.Kind != KindActivity {
		s := e.Label
		var notes []string
		if e.SubLabel != "" {
			notes = append(notes, e.SubLabel)
		}
		if e.Plate != "" {
			notes = append(notes, "plate "+e.Plate)
		}
		if len(notes) > 0 {
			s += " (" + strings.Join(notes, "; ") + ")"
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

// ident is a vehicle's identity annotation: the known-plate name
// Frigate put in SubLabel ("Jeff's BMW") when there is one, else the
// raw plate read.
func ident(e Event) string {
	if e.SubLabel != "" {
		return e.SubLabel
	}
	if e.Plate != "" {
		return "plate " + e.Plate
	}
	return ""
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
