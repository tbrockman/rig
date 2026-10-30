package agent

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// Each threshold reads one fact off the probe, says whether it is past the
// limit, and says when it cannot tell rather than guess.
func TestEvaluateReadsEachThresholdOffTheProbe(t *testing.T) {
	now := int64(1793390400)
	p := Probe{Now: now, Head: now - 130*60, StatusMD: "812." + strconv.FormatInt(now-95*60, 10),
		Units:     []TransientUnit{{Name: "mono-warm.service", Since: now - 52*60}, {Name: "dev.service", Since: now - 5*60}},
		Listening: map[int]bool{5173: true}}
	thr := Thresholds{Idle: 120 * time.Minute, Unit: 30 * time.Minute, Status: 90 * time.Minute,
		Usage: &UsageLimit{Window: "five_hour", Max: 0.7}, Forwards: []int{9222, 5173}}
	q := &Quota{Windows: []Window{{Name: "five_hour", Utilization: 0.72}}}

	got := Evaluate(thr, p, q, true)
	want := []string{
		"idle/ OVER 130m/120m", "unit/dev.service ok 5m/30m", "unit/mono-warm.service OVER 52m/30m",
		"status/ OVER 95m/90m", "usage/five_hour OVER 0.72/0.70",
		"forward/5173 ok listening/listening", "forward/9222 OVER nothing listens on it in the guest/listening",
	}
	var lines []string
	for _, r := range got {
		state := "ok"
		if r.Over {
			state = "OVER"
		}
		if !r.Known {
			state = "?"
		}
		lines = append(lines, r.Key()+" "+state+" "+r.Value+"/"+r.Limit)
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}

	// Facts that cannot be read are unknown, not under.
	bare := Evaluate(thr, Probe{Now: now}, nil, false)
	for _, r := range bare {
		switch r.Name {
		case ThrForward:
			if !r.Over || r.Value != "holder not running on the host" {
				t.Errorf("no holder is a forward down: %+v", r)
			}
		case ThrUnit:
			t.Errorf("no unit, no reading: %+v", r)
		default:
			if r.Known {
				t.Errorf("%s must be unknown with nothing to read: %+v", r.Name, r)
			}
		}
	}
}

// A crossing is said once, a clearing once, and a watcher that starts with a
// threshold already crossed hears of it; what cannot be read says nothing.
func TestThresholdEventsSayCrossingsAndClearingsOnce(t *testing.T) {
	over := []Reading{{Name: ThrIdle, Known: true, Over: true, Value: "130m", Limit: "120m"},
		{Name: ThrUnit, Detail: "x.service", Known: true, Over: false, Value: "5m", Limit: "30m"},
		{Name: ThrUsage, Detail: "five_hour", Known: false, Value: "no quota"}}
	first := ThresholdEvents(nil, over)
	if len(first) != 1 || first[0].Type != EvIdle || first[0].Value("state") != "crossed" || first[0].Value("age") != "130m" {
		t.Fatalf("got %+v", first)
	}
	if again := ThresholdEvents(over, over); len(again) != 0 {
		t.Errorf("nothing changed, yet %+v", again)
	}
	under := []Reading{{Name: ThrIdle, Known: true, Over: false, Value: "1m", Limit: "120m"},
		{Name: ThrUnit, Detail: "x.service", Known: true, Over: true, Value: "31m", Limit: "30m"},
		{Name: ThrUsage, Detail: "five_hour", Known: true, Over: false, Value: "0.1", Limit: "0.70"}}
	evs := ThresholdEvents(over, under)
	if types(evs) != EvIdle+","+EvLongUnit || evs[0].Value("state") != "cleared" || evs[1].Value("unit") != "x.service" {
		t.Errorf("got %+v", evs)
	}
	// Forwards have their own pair of names.
	down := []Reading{{Name: ThrForward, Detail: "5173", Known: true, Over: true, Value: "nothing listens"}}
	up := []Reading{{Name: ThrForward, Detail: "5173", Known: true, Over: false, Value: "listening"}}
	if e := ThresholdEvents(nil, down); len(e) != 1 || e[0].Type != EvForwardDown || e[0].Value("port") != "5173" {
		t.Errorf("got %+v", e)
	}
	if e := ThresholdEvents(down, up); len(e) != 1 || e[0].Type != EvForwardUp {
		t.Errorf("got %+v", e)
	}
}

func TestQuotaWindowAndString(t *testing.T) {
	var none *Quota
	if _, ok := none.Window("five_hour"); ok || !strings.Contains(none.String(), "no quota") {
		t.Error("a nil quota has no window and says so")
	}
	q := &Quota{Windows: []Window{{Name: "seven_day", Utilization: 0.55}, {Name: "five_hour", Utilization: 0.19, ResetsAt: time.Now().Add(2 * time.Hour)}}}
	if w, ok := q.Window("five_hour"); !ok || w.Utilization != 0.19 {
		t.Errorf("got %+v %v", w, ok)
	}
	s := q.String()
	if !strings.HasPrefix(s, "five_hour") || !strings.Contains(s, "19%") || !strings.Contains(s, "resets") || !strings.Contains(s, "seven_day           55%") {
		t.Errorf("got\n%s", s)
	}
}
