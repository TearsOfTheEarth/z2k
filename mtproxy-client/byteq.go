package main

import "sync"

// Оптимизированная очередь без спавна горутин и с преаллокацией
type byteQueue struct {
	mu     sync.Mutex
	frames [][]byte
	bytes  int64
	capB   int64
	closed bool
	done   bool
	notify chan struct{} // Канал для пробуждения без аллокаций
}

func newByteQueue(capBytes int64) *byteQueue {
	return &byteQueue{
		capB:   capBytes,
		frames: make([][]byte, 0, 128), // Преаллокация избавляет от реаллокаций
		notify: make(chan struct{}, 1),  // Буфер 1, чтобы не терять сигнал при push
	}
}

func (q *byteQueue) push(f []byte) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.done || q.bytes+int64(len(f)) > q.capB {
		return false
	}
	q.frames = append(q.frames, f)
	q.bytes += int64(len(f))
	select {
	case q.notify <- struct{}{}:
	default:
	}
	return true
}

func (q *byteQueue) pop() ([]byte, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.frames) == 0 {
		return nil, false
	}
	f := q.frames[0]
	q.frames[0] = nil // Избегаем memory leak, позволяя GC забрать []byte
	q.frames = q.frames[1:]
	q.bytes -= int64(len(f))
	return f, true
}

// wait: Переписан без спавна горутин. Безопасен для single-consumer.
func (q *byteQueue) wait(done <-chan struct{}) bool {
	for {
		q.mu.Lock()
		if len(q.frames) > 0 {
			q.mu.Unlock()
			return true
		}
		if q.closed || q.done {
			q.mu.Unlock()
			return false
		}
		q.mu.Unlock()

		select {
		case <-done:
			return false
		case <-q.notify:
			// Проснулись, идем проверять наличие кадров под локом
		}
	}
}

func (q *byteQueue) finish() {
	q.mu.Lock()
	q.done = true
	q.mu.Unlock()
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

func (q *byteQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.frames = nil
	q.bytes = 0
	q.mu.Unlock()
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

func (q *byteQueue) queued() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.bytes
}

func (q *byteQueue) setCap(n int64) {
	q.mu.Lock()
	q.capB = n
	q.mu.Unlock()
}

func (q *byteQueue) isClosed() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.closed
}
