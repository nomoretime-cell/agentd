// The API supplies RFC 3339 in the server's zone, including the offset at
// the event's date. Rendering those fields also works for Go's "Local" zone.
export function serverTime(value: string, timeOnly = false) {
  const parts =
    /^(\d{4}-\d{2}-\d{2})T(\d{2}:\d{2}:\d{2})(?:\.\d+)?(Z|[+-]\d{2}:\d{2})$/.exec(
      value
    )
  if (!parts) return value || "历史未记录"
  const [, date, time, offset] = parts
  return (
    (timeOnly ? "" : date + " ") +
    time +
    " UTC" +
    (offset === "Z" ? "" : offset)
  )
}
