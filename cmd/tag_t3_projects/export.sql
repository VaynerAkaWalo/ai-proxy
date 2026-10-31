-- Run against T3's statev2.sqlite: sqlite3 -json statev2.sqlite < export.sql > t3.json
select
  json_extract(pt.payload_json, '$.nativeThreadRef.nativeId') as native_id,
  coalesce(json_extract(t.payload_json, '$.worktreePath'), p.workspace_root) as cwd,
  (
    select json_group_array(json_array(r.requested_at, r.completed_at, json_extract(r.payload_json, '$.modelSelection.model')))
    from orchestration_v2_projection_runs r
    where r.provider_thread_id = pt.provider_thread_id and r.completed_at is not null
  ) as runs
from orchestration_v2_projection_provider_threads pt
join orchestration_v2_projection_threads t on t.thread_id = pt.thread_id
left join projection_projects p on p.project_id = t.project_id
where json_extract(pt.payload_json, '$.nativeThreadRef.nativeId') is not null
union all
-- Chats from before T3 recorded native threads keep their session only in the resume cursor.
select * from (
  select
    case provider_name
      when 'claudeAgent' then json_extract(resume_cursor_json, '$.resume')
      else json_extract(resume_cursor_json, '$.threadId')
    end as native_id,
    json_extract(runtime_payload_json, '$.cwd') as cwd,
    '[]' as runs
  from provider_session_runtime
)
where native_id is not null;
