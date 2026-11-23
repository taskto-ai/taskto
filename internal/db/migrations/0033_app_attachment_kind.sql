-- What an app attachment was uploaded to be: a photo of an attachment field, or the picture of a
-- signature field (internal/apps/attachments.go, capture.go).
--
-- A record claims an attachment only for a field of the kind it was uploaded for, so a field that is
-- retyped (a photo field made a signature field, say) can't take the pending uploads of what it was
-- before: a photo never becomes a signature, which no later change can alter.
--
-- Attachments from before this column are photos.

ALTER TABLE app_attachments ADD COLUMN kind TEXT NOT NULL DEFAULT 'attachment' CHECK (kind IN ('attachment', 'signature'));
