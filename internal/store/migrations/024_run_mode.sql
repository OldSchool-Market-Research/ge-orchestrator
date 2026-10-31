-- A run is a full research cycle or the judging of a script-built packet. A lane's
-- packet is then either logged beside the run or what the run was given.
ALTER TABLE orchestrator.runs
  ADD COLUMN mode text NOT NULL DEFAULT 'research' CHECK (mode IN ('research', 'evaluate'));

ALTER TABLE orchestrator.lane_packets DROP CONSTRAINT lane_packets_mode_check;
ALTER TABLE orchestrator.lane_packets
  ADD CONSTRAINT lane_packets_mode_check CHECK (mode IN ('log', 'live'));
