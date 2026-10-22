# Run with jq -ne -f check-guest-stats.jq observations.jsonl.
# Streaming read: a latest error must fail, not silently reuse an older success.
reduce inputs as $sample (null; $sample)
| . as $last
| now as $now
| def fresh:
    if type != "string" then false else
    (try (sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601) catch null) as $time
    | $time != null and ($now - $time >= 0) and ($now - $time < 45) end;
  [ $last.summary.node,
    $last.summary.node.systemContainers[]?,
    $last.summary.pods[]?,
    $last.summary.pods[]?.containers[]? ] as $series
| {observedAt:$last.observedAt,status:$last.status,node:$last.node,
   resourceSeries:($series | length),
   fresh:($last.status == "success" and ($last.observedAt | fresh)
          and ($series | length > 0)
          and all($series[]; (.cpu.time | fresh) and (.memory.time | fresh)
                  and (.cpu.usageCoreNanoSeconds | type == "number")
                  and (.memory.workingSetBytes | type == "number"))),
   qualified:false}
| if .fresh then . else ., false end
