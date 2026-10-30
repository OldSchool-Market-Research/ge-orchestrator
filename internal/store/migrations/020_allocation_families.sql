-- Lane C's yields per recipe family (ge-data init/07), next to lanes and sources.
ALTER TABLE orchestrator.lane_allocation ADD COLUMN families jsonb NOT NULL DEFAULT '[]';
