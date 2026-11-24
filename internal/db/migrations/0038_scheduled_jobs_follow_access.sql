-- A job runs as its owner and needs their access to its space (or, with no space, to its
-- workspace): a job whose owner has lost that fails when it comes due, and until then it holds a
-- place of the owner's bound on waiting jobs. So the jobs go with the access, in the transaction
-- that ends it, whatever code ends it.

-- Leaving a shared space ends the owner's jobs of that space.
CREATE TRIGGER space_members_drop_jobs
AFTER DELETE ON space_members
BEGIN
    DELETE FROM scheduled_jobs WHERE user_id = OLD.user_id AND space_id = OLD.space_id;
END;

-- Leaving a workspace ends all the owner's jobs in it, those of their private space included.
CREATE TRIGGER memberships_drop_jobs
AFTER DELETE ON memberships
BEGIN
    DELETE FROM scheduled_jobs WHERE user_id = OLD.user_id AND workspace_id = OLD.workspace_id;
END;
