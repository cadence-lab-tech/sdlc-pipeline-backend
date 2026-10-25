package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// failures — сбои подряд по прогону (CLAB-215).
//
// Упавшего агента раннер отпускает, и без учёта сбоев следующий опрос забрал
// бы тот же прогон снова: на подписке, где типичная причина сбоя — лимит,
// раннер молотил бы очередь пустыми запусками. Ключ — прогон, а не задача:
// задача, вернувшаяся в статус, получает новый прогон, и её счёт начинается
// с нуля. Состояние живёт в памяти раннера: перезапуск — способ владельца
// вернуть пропущенный прогон.
type failures struct {
	max        int
	base, ceil time.Duration

	mu   sync.Mutex
	runs map[string]failureState
}

type failureState struct {
	count int
	// Не забирать раньше этого момента; после max-го сбоя — никогда.
	until   time.Time
	skipped bool
}

// failureVerdict — что раннер решил после сбоя: номер сбоя из max и когда
// прогон можно забрать снова.
type failureVerdict struct {
	n, max  int
	delay   time.Duration
	skipped bool
}

func newFailures(limit int, base, ceil time.Duration) *failures {
	return &failures{max: limit, base: base, ceil: ceil, runs: map[string]failureState{}}
}

// blocked — прогон на паузе после сбоя или пропущен до перезапуска.
func (f *failures) blocked(run string, now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.runs[run]
	return ok && (st.skipped || now.Before(st.until))
}

// fail записывает сбой и отвечает, что дальше: пауза
// min(base·2^(n-1), ceil) или пропуск после max-го сбоя подряд.
func (f *failures) fail(run string, now time.Time) failureVerdict {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.runs[run]
	st.count++
	v := failureVerdict{n: st.count, max: f.max}
	if st.count >= f.max {
		st.skipped, v.skipped = true, true
	} else {
		v.delay = f.delay(st.count)
		st.until = now.Add(v.delay)
	}
	f.runs[run] = st
	return v
}

func (f *failures) delay(n int) time.Duration {
	d := f.base
	for i := 1; i < n && d < f.ceil; i++ {
		d *= 2
	}
	return min(d, f.ceil)
}

// holdFrom отсчитывает паузу заново от now — от момента, когда release
// дошёл до сервера: пауза обещана от отпуска, а не от решения о нём.
func (f *failures) holdFrom(run string, now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if st, ok := f.runs[run]; ok && !st.skipped {
		st.until = now.Add(f.delay(st.count))
		f.runs[run] = st
	}
}

// reset — успешный прогон: редкие сбои не копятся до пропуска.
func (f *failures) reset(run string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.runs, run)
}

// note — хвост причины release: номер сбоя и что будет дальше.
func (v failureVerdict) note() string {
	if v.skipped {
		return fmt.Sprintf("сбой %d из %d, пропускаю до перезапуска раннера", v.n, v.max)
	}
	return fmt.Sprintf("сбой %d из %d, повторный захват не раньше чем через %s", v.n, v.max, shortDuration(v.delay))
}

// shortDuration — длительность без нулевых хвостов: 1m, а не 1m0s; её
// читает человек в ленте задачи.
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
