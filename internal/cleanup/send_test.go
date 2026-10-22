package cleanup

import "testing"

func TestParseCommandSend(t *testing.T) {
	cases := []struct {
		name, in              string
		profile, adHoc, strip string
		found, send           bool
	}{
		{"bare send after pause", "τα λέμε αύριο, τέλος μηνύματος.", "", "", "τα λέμε αύριο", true, true},
		{"bare send, no pause needed", "τα λέμε αύριο τέλος μηνύματος", "", "", "τα λέμε αύριο", true, true},
		{"case, accents, punct", "τα λέμε αύριο. Τέλος, Μηνυματος!", "", "", "τα λέμε αύριο", true, true},
		{"profile then send", "τα λέμε αύριο, εντολή μήνυμα τέλος μηνύματος.", "message", "", "τα λέμε αύριο", true, true},
		{"ad-hoc then send", "τα λέμε αύριο, εντολή κάνε το επίσημο τέλος μηνύματος", "", "κάνε το επίσημο", "τα λέμε αύριο", true, true},
		{"old command is no longer send", "τα λέμε αύριο, εντολή στείλε.", "", "στείλε", "τα λέμε αύριο", true, false},
		{"phrase mid-text is not send", "στο τέλος μηνύματος βάλε υπογραφή", "", "", "στο τέλος μηνύματος βάλε υπογραφή", false, false},
		{"μηνύματος alone is not send", "η αρχή του μηνύματος", "", "", "η αρχή του μηνύματος", false, false},
		{"no command", "απλό κείμενο", "", "", "απλό κείμενο", false, false},
		{"only the phrase", "τέλος μηνύματος", "", "", "", true, true},
		{"profile switch still works", "κείμενο, εντολή μήνυμα.", "message", "", "κείμενο", true, false},
		{"no-break space", "τα λέμε αύριο,\u00a0τέλος\u00a0μηνύματος", "", "", "τα λέμε αύριο", true, true},
		{"greek question mark", "τα λέμε αύριο; τέλος μηνύματος\u037e", "", "", "τα λέμε αύριο", true, true},
		{"profile send with no-break space", "κείμενο, εντολή\u00a0μήνυμα τέλος μηνύματος", "message", "", "κείμενο", true, true},
		{"decomposed accents (NFD)", "τα λέμε αύριο, τε\u0301λος μηνυ\u0301ματος", "", "", "τα λέμε αύριο", true, true},
		{"empty", "", "", "", "", false, false},
		{"whitespace only", "  \n", "", "", "  \n", false, false},
	}
	for _, c := range cases {
		p, a, s, f, snd := ParseCommandSend(c.in, "εντολή", sampleProfiles())
		if p != c.profile || a != c.adHoc || s != c.strip || f != c.found || snd != c.send {
			t.Errorf("%s: got (%q,%q,%q,%v,%v) want (%q,%q,%q,%v,%v)",
				c.name, p, a, s, f, snd, c.profile, c.adHoc, c.strip, c.found, c.send)
		}
	}
}
