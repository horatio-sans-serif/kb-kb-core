// Package review is the spaced-repetition engine (design §8.11): an SM-2 variant
// over the review_schedule table. A recall grade lengthens the interval for items
// recalled well and resets items that lapse, so the surfacing layer can present a
// daily review queue of what's due.
package review

import "math"

// minEase is the SM-2 ease-factor floor; below it intervals would grow too slowly
// to be useful, so the algorithm clamps here.
const minEase = 1.3

// maxIntervalDays caps the review interval. Canonical SM-2 leaves the interval
// unbounded, but here it is converted to an int64-nanosecond Duration
// (next_review_at = now + interval), which overflows and wraps NEGATIVE past
// ~292 years — making an item that was recalled perfectly suddenly, permanently
// due. 100 years is far past any real review horizon and leaves the duration math
// well inside int64.
const maxIntervalDays = 36500

// State is an artifact's spaced-repetition schedule, mirroring the persisted
// review_schedule row minus the next_review_at timestamp (the service derives
// that from a clock).
type State struct {
	IntervalDays float64
	Ease         float64
	Reviews      int // count of consecutive successful repetitions
}

// Default is the initial state for a never-reviewed artifact.
func Default() State { return State{IntervalDays: 0, Ease: 2.5, Reviews: 0} }

// Schedule applies one SM-2 update for a recall grade (0..5, clamped) and returns
// the next state. The ease factor is adjusted for every grade and floored at
// minEase. A grade below 3 is a lapse: the repetition count and interval reset to
// re-learn the item, though the ease still decays. A grade of 3+ advances: the
// first success is a 1-day interval, the second 6 days, and thereafter the
// previous interval times the (post-update) ease, rounded to whole days.
func Schedule(prev State, grade int) State {
	grade = clampGrade(grade)

	next := prev
	next.Ease = updateEase(prev.Ease, grade)

	if grade < 3 {
		next.Reviews = 0
		next.IntervalDays = 1
		return next
	}

	next.Reviews = prev.Reviews + 1
	switch next.Reviews {
	case 1:
		next.IntervalDays = 1
	case 2:
		next.IntervalDays = 6
	default:
		// max(round(prev*ease), prev+1): guarantee the interval strictly grows even
		// at the ease floor, where round(1 * 1.3) == 1 would otherwise pin a
		// recovered item at a 1-day interval forever.
		grown := math.Round(prev.IntervalDays * next.Ease)
		if grown < prev.IntervalDays+1 {
			grown = prev.IntervalDays + 1
		}
		next.IntervalDays = grown
	}
	if next.IntervalDays > maxIntervalDays {
		next.IntervalDays = maxIntervalDays
	}
	return next
}

// updateEase applies the SM-2 ease adjustment for a grade and clamps at the floor.
func updateEase(ease float64, grade int) float64 {
	q := float64(grade)
	ease += 0.1 - (5-q)*(0.08+(5-q)*0.02)
	if ease < minEase {
		return minEase
	}
	return ease
}

func clampGrade(g int) int {
	switch {
	case g < 0:
		return 0
	case g > 5:
		return 5
	default:
		return g
	}
}
