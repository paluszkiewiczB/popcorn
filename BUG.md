# TOCTOURace Test Failure — Root Cause Analysis

## The Test

```
StartBuffering()
Subscribe("a", ch)   // ch buffer = 10
wg.Go 20×: Send(ctx, e)
wg.Go 1×:  FinishBuffering()
wg.Wait()
for 20: read from ch with 1s timeout
```

## The Symptom

Only 10 events arrive in ch. Test blocks at event #11 and times out.
The channel has exactly `cap(ch)` events — only the direct-delivery successes.

## The Implementation

**Send (goroutine):**
```
1. bufMu.Lock → if buffering { append to buf } → bufMu.Unlock
2. deliverToAll(sendCtx, event):
     mu.RLock → copy listeners → mu.RUnlock
     for each listener: sendEvent(sendCtx, ch, e)
       select { case <-sendCtx.Done(): timeout; case ch <- e: success }
```

**FinishBuffering (flusher goroutine):**
```
1. bufMu.Lock → remaining = buf; buf = nil; buffering = false → bufMu.Unlock
2. mu.RLock → copy channels into chans → mu.RUnlock
3. for each event in remaining: go func() { for _, ch := range chans { ch <- evt } }
4. runtime.Gosched()
```

## What I Know

1. **All 21 goroutines (20 Sends + 1 flusher) complete wg.Wait.** The test proceeds to reading.
2. **10 events are in ch** (from successful direct deliveries).
3. **FinishBuffering's goroutines are spawned** but deliver 0 events to ch.

## What I Don't Know (and can't figure out)

### Question 1: Is the buffer empty when FinishBuffering captures it?

If the flusher goroutine wins the `bufMu` race against all 20 Sends (runs before any Send has called `bufMu.Lock`), `remaining` is empty → 0 drain goroutines spawned → only 10 direct-delivery events in ch.

But the Sends are spawned BEFORE the flusher (loop at lines 228-233, then flusher at line 235). On the test goroutine's first yield (wg.Wait), the Sends are at the front of the run queue.

Fact: runtime.Gosched() in FinishBuffering should yield CPU to the Sends after spawning. If they run, they buffer events. Then the buffer should be non-empty.

### Question 2: If the buffer is non-empty, why don't the drain goroutines deliver?

Each drain goroutine does: `for _, ch := range chans { ch <- evt }`. No locks held before the send. `chans` was captured from `b.listeners` under `mu.RLock`. It contains one entry: the test's `ch`.

When the test reads event N from ch (buffer goes 10→9), one drain goroutine's `ch <- evt` unblocks. Buffer goes back to 10. Test reads again. Repeat.

This should produce 20 events (10 direct + 10 drain). But it doesn't.

Attempted with `runtime.Gosched()` — no change. Attempted with different GOMAXPROCS — no change.

### Question 3: Is there a subtle interaction with the send timeouts?

The 10 timed-out Sends are blocked on `sendEvent`'s select:
```
select { case <-sendCtx.Done(): timeout; case ch <- e: success }
```

They execute concurrently with FinishBuffering's drain goroutines. Both types of goroutines are parked on `ch <- e` / `ch <- evt`. When the test reads, the Go runtime wakes ONE parked writer.

The drain goroutines use `context.Background()` (no timeout). The Send goroutines use `sendCtx` (1s timeout). After the timeout fires, the Send's `<-sendCtx.Done()` case wins and the goroutine returns.

After all timeouts have fired (~1s), only drain goroutines remain parked on ch. The test should then be able to drain them.

This timing seems correct. The test has a 1s timeout per event read — 20 events would take max 20s. Plenty of time.

## Summary

**Most likely cause:** The buffer is empty (0 events) when FinishBuffering captures it, because the flusher goroutine runs before any Send goroutine.

**Why this contradicts the code:** The Sends are spawned 18 lines before the flusher. By Go scheduling conventions, the Sends should be scheduled first.

**What contradicts this theory:** `runtime.Gosched()` would force the flusher to yield after spawning drain goroutines. If the Sends haven't run yet, Gosched would let them run. But even with Gosched, the test fails.

I need help resolving this contradiction. I suspect there's something about Go's goroutine scheduling or the channel interactions that I'm missing.
