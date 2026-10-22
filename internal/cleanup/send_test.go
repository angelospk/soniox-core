package cleanup

import "testing"

func TestParseCommandSend(t *testing.T) {
	cases := []struct {
		name, in              string
		profile, adHoc, strip string
		found, send           bool
	}{
		{"bare send after pause", "τα λέμε αύριο, εντολή στείλε.", "", "", "τα λέμε αύριο", true, true},
		{"bare send, no pause needed", "τα λέμε αύριο εντολή στείλε", "", "", "τα λέμε αύριο", true, true},
		{"case, accents, punct", "τα λέμε αύριο. Εντολή, Στειλε!", "", "", "τα λέμε αύριο", true, true},
		{"profile then send", "τα λέμε αύριο, εντολή μήνυμα στείλε.", "message", "", "τα λέμε αύριο", true, true},
		{"ad-hoc then send", "τα λέμε αύριο, εντολή κάνε το επίσημο στείλε", "", "κάνε το επίσημο", "τα λέμε αύριο", true, true},
		{"plain trailing στείλε is speech", "πες του να στο στείλε.", "", "", "πες του να στο στείλε.", false, false},
		{"στείλε mid-command is not send", "κείμενο, εντολή στείλε το επίσημα", "", "στείλε το επίσημα", "κείμενο", true, false},
		{"no command", "απλό κείμενο", "", "", "απλό κείμενο", false, false},
		{"only the command", "εντολή στείλε", "", "", "", true, true},
		{"profile switch still works", "κείμενο, εντολή μήνυμα.", "message", "", "κείμενο", true, false},
		{"no-break space", "τα λέμε αύριο,\u00a0εντολή\u00a0στείλε", "", "", "τα λέμε αύριο", true, true},
		{"greek question mark", "τα λέμε αύριο; εντολή στείλε\u037e", "", "", "τα λέμε αύριο", true, true},
		{"profile send with no-break space", "κείμενο, εντολή\u00a0μήνυμα στείλε", "message", "", "κείμενο", true, true},
		{"decomposed accents (NFD)", "τα λέμε αύριο, εντολη\u0301 στει\u0301λε", "", "", "τα λέμε αύριο", true, true},
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
