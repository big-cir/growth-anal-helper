// Language of the screen: the server sets <html lang>.
const KO = typeof document !== 'undefined' && document.documentElement.lang === 'ko';

/** Give English and Korean; the configured language is used */
export const tr = (en, ko) => (KO ? ko : en);
export const LANG = KO ? 'ko' : 'en';
