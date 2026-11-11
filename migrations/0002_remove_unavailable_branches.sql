-- Explicitly approved removals: these advertised branches return no task data.
DELETE FROM chat_branches WHERE branch IN ('c9f1', 'c8', 'c7.1', 'c7');
DELETE FROM rdb_branches WHERE branch IN ('c9f1', 'c8', 'c7.1', 'c7');
