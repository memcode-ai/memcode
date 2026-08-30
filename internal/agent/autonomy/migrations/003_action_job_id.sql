-- A delegated action spawns a detached job, and check_delegate has to find its
-- way back from the job id to the action it must close out. That mapping used
-- to be written into the facts table ("delegation.<job-id>"), abusing a
-- semantic-memory store as a keyed index because it was the only durable place
-- delegate could write without a migration. This is that migration: the link
-- belongs on the action itself.
ALTER TABLE actions ADD COLUMN job_id TEXT;
CREATE INDEX IF NOT EXISTS actions_job ON actions(job_id) WHERE job_id IS NOT NULL;
