import { useLayoutEffect, useState } from "react"
import { Check, Moon, Sun } from "lucide-react"
import { DropdownMenu } from "radix-ui"

const themes = {
  dark: "深色",
  light: "浅色",
  cyberpunk: "赛博朋克（深色）",
  "cyberpunk-light": "赛博朋克（浅色）",
  system: "跟随系统",
} as const
type Theme = keyof typeof themes
const storageKey = "agentd-theme"

function isTheme(value: string | null): value is Theme {
  return value !== null && Object.hasOwn(themes, value)
}

function readTheme(): Theme {
  try {
    const saved = localStorage.getItem(storageKey)
    if (isTheme(saved)) return saved
  } catch {
    // The preference still works for this page when storage is unavailable.
  }
  return "dark"
}

export function ThemeMenu() {
  const [theme, setTheme] = useState<Theme>(readTheme)
  useLayoutEffect(() => {
    const media = window.matchMedia("(prefers-color-scheme: dark)")
    const classes = document.documentElement.classList
    document.documentElement.dataset.theme = theme
    const apply = () => {
      // index.html applies the same rule before first paint.
      const light =
        theme === "light" ||
        theme === "cyberpunk-light" ||
        (theme === "system" && !media.matches)
      classes.toggle("dark", !light)
    }
    apply()
    if (theme !== "system") return
    media.addEventListener("change", apply)
    return () => media.removeEventListener("change", apply)
  }, [theme])
  return (
    <DropdownMenu.Root modal={false}>
      <DropdownMenu.Trigger asChild>
        <button
          className="theme-trigger"
          aria-label="切换主题"
          title="切换主题"
        >
          <Sun size={18} className="dark:hidden" />
          <Moon size={18} className="hidden dark:block" />
        </button>
      </DropdownMenu.Trigger>
      <DropdownMenu.Portal>
        <DropdownMenu.Content className="theme-menu" align="end" sideOffset={5}>
          <DropdownMenu.RadioGroup
            value={theme}
            onValueChange={(next) => {
              if (!isTheme(next)) return
              setTheme(next)
              try {
                localStorage.setItem(storageKey, next)
              } catch {
                /* Storage is optional. */
              }
            }}
          >
            {Object.entries(themes).map(([value, label]) => (
              <DropdownMenu.RadioItem
                key={value}
                value={value}
                className="theme-choice"
              >
                {label}
                <DropdownMenu.ItemIndicator>
                  <Check size={15} />
                </DropdownMenu.ItemIndicator>
              </DropdownMenu.RadioItem>
            ))}
          </DropdownMenu.RadioGroup>
        </DropdownMenu.Content>
      </DropdownMenu.Portal>
    </DropdownMenu.Root>
  )
}
