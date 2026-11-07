-- The description of an application: the sentence the administrator writes
-- for the site, shown beside its name in the console.

ALTER TABLE apps ADD COLUMN description TEXT NOT NULL DEFAULT '';
