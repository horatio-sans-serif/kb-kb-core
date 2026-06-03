package review

import (
	"math"
	"testing"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestSchedule_FirstSuccessfulReviews(t *testing.T) {
	// First successful rep: interval 1 day, reps 1, ease bumped (+0.1 at q=5).
	s := Schedule(Default(), 5)
	if s.Reviews != 1 || !approx(s.IntervalDays, 1) || !approx(s.Ease, 2.6) {
		t.Errorf("first review = %+v, want reviews 1, interval 1, ease 2.6", s)
	}
	// Second successful rep: interval jumps to 6 days, reps 2.
	s2 := Schedule(s, 5)
	if s2.Reviews != 2 || !approx(s2.IntervalDays, 6) {
		t.Errorf("second review = %+v, want reviews 2, interval 6", s2)
	}
	// Third rep: interval = round(prev interval * ease).
	s3 := Schedule(State{IntervalDays: 6, Ease: 2.5, Reviews: 2}, 4)
	if s3.Reviews != 3 || !approx(s3.IntervalDays, 15) { // round(6 * 2.5)
		t.Errorf("third review = %+v, want reviews 3, interval 15", s3)
	}
}

func TestSchedule_EaseUpdateByGrade(t *testing.T) {
	cases := map[int]float64{5: 2.6, 4: 2.5, 3: 2.36}
	for grade, wantEase := range cases {
		s := Schedule(State{IntervalDays: 1, Ease: 2.5, Reviews: 1}, grade)
		if !approx(s.Ease, wantEase) {
			t.Errorf("grade %d: ease = %.4f, want %.4f", grade, s.Ease, wantEase)
		}
	}
}

func TestSchedule_LapseResets(t *testing.T) {
	// A failing grade (<3) restarts: reps 0, interval 1, ease still decays.
	prev := State{IntervalDays: 30, Ease: 2.5, Reviews: 5}
	s := Schedule(prev, 1)
	if s.Reviews != 0 || !approx(s.IntervalDays, 1) {
		t.Errorf("lapse = %+v, want reps 0, interval 1", s)
	}
	if s.Ease >= prev.Ease {
		t.Errorf("lapse should decay ease, got %.4f (was %.4f)", s.Ease, prev.Ease)
	}
}

func TestSchedule_EaseFloor(t *testing.T) {
	// Repeated poor grades cannot drive ease below the 1.3 floor.
	s := State{IntervalDays: 1, Ease: 1.3, Reviews: 1}
	for range 10 {
		s = Schedule(s, 0)
	}
	if s.Ease < 1.3 {
		t.Errorf("ease fell below the 1.3 floor: %.4f", s.Ease)
	}
}

func TestSchedule_GradeClamped(t *testing.T) {
	// Out-of-range grades are clamped, not treated as huge eases.
	hi := Schedule(Default(), 99)
	lo := Schedule(Default(), -5)
	if hi.Reviews != 1 { // clamped to 5 → a success
		t.Errorf("grade 99 should clamp to a success, got %+v", hi)
	}
	if lo.Reviews != 0 { // clamped to 0 → a lapse
		t.Errorf("grade -5 should clamp to a lapse, got %+v", lo)
	}
}
