// getRandomValues also works on the trusted LAN's HTTP origin.
export function requestKey() {
  return Array.from(crypto.getRandomValues(new Uint8Array(16)), (b) =>
    b.toString(16).padStart(2, "0")
  ).join("")
}

export function pendingKey(storageKey: string, value?: string) {
  try {
    if (value === undefined) return sessionStorage.getItem(storageKey) || ""
    if (value) sessionStorage.setItem(storageKey, value)
    else sessionStorage.removeItem(storageKey)
  } catch {
    // The caller retains the key in memory when browser storage is unavailable.
  }
  return ""
}
