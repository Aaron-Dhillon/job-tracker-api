package auth_test

import "time"

// timeIt reports how long fn took. Used only to prove DummyCheck does real
// bcrypt work; nothing here asserts an absolute duration, which would be
// flaky on a loaded CI runner.
func timeIt(fn func()) time.Duration {
	start := time.Now()
	fn()
	return time.Since(start)
}
