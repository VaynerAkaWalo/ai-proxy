BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;
SELECT json_build_object('export_total_rows', count(*))::text FROM "LiteLLM_SpendLogs";
SELECT json_build_object(
  'request_id', request_id,
  'call_type', call_type,
  'provider', custom_llm_provider,
  'model', model,
  'requested_model', model_group,
  'client_id', CASE WHEN api_key ~ '^[0-9a-f]{64}$' THEN api_key END,
  'session_id', session_id,
  'started_at', to_char("startTime", 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
  'completed_at', to_char("endTime", 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
  'status', status,
  'spend', spend::text,
  'prompt_tokens', prompt_tokens,
  'completion_tokens', completion_tokens,
  'total_tokens', total_tokens,
  'cache_read_tokens', metadata#>'{usage_object,prompt_tokens_details,cached_tokens}',
  'cache_write_tokens', coalesce(nullif(metadata#>'{usage_object,prompt_tokens_details,cache_creation_tokens}', 'null'::jsonb), nullif(metadata#>'{usage_object,prompt_tokens_details,cache_write_tokens}', 'null'::jsonb)),
  'reasoning_tokens', metadata#>'{usage_object,completion_tokens_details,reasoning_tokens}',
  'cost_kind', metadata#>>'{spend_logs_metadata,cost_kind}',
  'proxy_schema', metadata#>'{spend_logs_metadata,accounting_schema}'
)::text
FROM "LiteLLM_SpendLogs"
ORDER BY "startTime", request_id;
COMMIT;
