# Keep the current model from turn_context and attach it to usage rows.
# Run with: cat *.jsonl | jq -n -r -f codex_usage.jq

def first_string($values):
  ([$values[] | select(type == "string" and length > 0)] | .[0]) // "";

def row_model($row):
  if $row.type == "turn_context" or $row.payload.type == "turn_context" then
    first_string([$row.payload.model, $row.payload.model_name])
  elif $row.type == "event_msg" and $row.payload.type == "token_count" then
    first_string([$row.payload.info.model, $row.payload.info.model_name,
                  $row.payload.model, $row.payload.model_name])
  else
    ""
  end;

foreach inputs as $row
  ("";
   if $row.type == "session_meta" then
     ""
   else
     row_model($row) as $model
     | if $model == "" then . else $model end
   end;
   if $row.type == "event_msg"
      and $row.payload.type == "token_count"
      and $row.payload.info.last_token_usage != null then
     $row.payload.info.last_token_usage as $u
     | [.,
        ($u.input_tokens // $u.prompt_tokens // $u.input // 0),
        ($u.output_tokens // $u.completion_tokens // $u.output // 0),
        ($u.cached_input_tokens // $u.cache_read_input_tokens // $u.cached_tokens // 0),
        ($u.cache_write_input_tokens // $u.cache_creation_input_tokens // 0),
        ($u.reasoning_output_tokens // $u.reasoning_tokens // 0),
        ($u.total_tokens // 0)]
     | @tsv
   else
     empty
   end)
