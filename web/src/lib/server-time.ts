// dateStyle and timeStyle cannot be combined with timeZoneName, so the
// fields are listed one by one.
const viewerTime = new Intl.DateTimeFormat("zh-CN", {
  year: "numeric",
  month: "2-digit",
  day: "2-digit",
  hour: "2-digit",
  minute: "2-digit",
  second: "2-digit",
  hourCycle: "h23",
  timeZoneName: "short",
})

// Instants without a server-zone rendering (Unix milliseconds or UTC
// timestamps) are shown in the viewer's zone, always with the zone named,
// in the same "date time zone" order as serverTime.
export function viewerInstant(value: number | string) {
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return String(value)
  const part = Object.fromEntries(
    viewerTime.formatToParts(date).map((p) => [p.type, p.value])
  )
  return `${part.year}-${part.month}-${part.day} ${part.hour}:${part.minute}:${part.second} ${part.timeZoneName}`
}

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
