// Language of user-facing text (workspace.json `language`).
export type Language = 'en' | 'ko';

let current: Language = 'en';

export function setLanguage(l: Language): void {
  current = l;
}

export function language(): Language {
  return current;
}

/** User-facing text: give both and the configured language is used */
export function tr(en: string, ko: string): string {
  return current === 'ko' ? ko : en;
}
