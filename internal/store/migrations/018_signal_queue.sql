-- Queue cleanup (research review, 2026-10-30). Over Oct 16-30, 93% of the
-- signals assigned to runs had been dismissed before and 0.45% became ships.
-- The collector stops re-queuing those (store.UpsertSignal's repeat guard) and
-- stops queuing lanes switched off in the brief; this withdraws what is already
-- waiting. The 'withdrawn:' prefix marks a harness close, so these rows do not
-- count as falsifications if the item comes back.

UPDATE orchestrator.signals
SET status='dismissed', reason='withdrawn: lane B is off (weight 0)', resolved_at=now()
WHERE kind='hvflip' AND status='pending';

UPDATE orchestrator.signals s
SET status='dismissed', reason='withdrawn: dismissed twice or more in 14 days', resolved_at=now()
WHERE s.status='pending' AND s.kind <> 'watch'
  AND coalesce(s.metrics->>'source','') <> 'confirm_rearm'
  AND (SELECT count(*) FROM orchestrator.signals d
       WHERE d.kind=s.kind AND d.item_id=s.item_id AND d.status='dismissed'
         AND d.resolved_at > now() - interval '14 days'
         AND coalesce(d.reason,'') <> 'expired unassigned'
         AND coalesce(d.reason,'') NOT LIKE 'withdrawn:%') >= 2;

-- The guard's lookups run once per flagged sweep row.
CREATE INDEX signals_dismissed_idx ON orchestrator.signals (kind, item_id, resolved_at DESC)
  WHERE status='dismissed';
