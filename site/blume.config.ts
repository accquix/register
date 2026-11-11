import { defineConfig } from "blume";
import { openapi } from "blume/reference";

const inter = { name: "Inter", provider: "google" as const, subsets: ["latin", "latin-ext"], weights: [400, 500, 600, 700] };
const onest = { name: "Onest", provider: "google" as const, subsets: ["latin", "latin-ext"], weights: [500, 600, 700] };

export default defineConfig({
  title: "Accquix Register",
  description:
    "Self-hosted search and lookup API for Slovak business register data: the Register of Legal Entities and the Financial Administration lists, mirrored into your PostgreSQL.",
  logo: {
    image: { light: "/logo-light.svg", dark: "/logo-dark.svg", alt: "Accquix Register" },
    text: "",
  },
  i18n: {
    defaultLocale: "en",
    locales: [
      { code: "en", label: "English" },
      { code: "sk", label: "Slovenčina" },
    ],
  },
  navigation: {
    tabs: [
      { label: { en: "Docs", sk: "Dokumentácia" }, path: "/", href: "/quickstart" },
      { label: { en: "API reference", sk: "API referencia" }, path: "/reference" },
      { label: { en: "Changelog", sk: "Zmeny" }, path: "/changelog" },
    ],
    repo: true,
  },
  github: { owner: "accquix", repo: "register", branch: "main", dir: "site/docs" },
  footer: {
    copyright: {
      en: "Code: Apache-2.0 · RPO © MV SR, CC BY 4.0 · Zdroj údajov: Finančná správa SR",
      sk: "Kód: Apache-2.0 · RPO © MV SR, CC BY 4.0 · Zdroj údajov: Finančná správa SR",
    },
    // The repository icon on the right already links to GitHub.
    links: [{ label: "Accquix", href: "https://accquix.com" }],
  },
  theme: {
    // Interfere Design System brand blue; light mode darkened for AA contrast.
    accent: { light: "#006ad6", dark: "#3c9fff" },
    radius: "md",
    mode: "system",
    toggle: true,
    fonts: { display: onest, body: inter, mono: "ibm-plex-mono" },
  },
  reference: [
    openapi({
      spec: "../api/openapi.yaml",
      // No public instance exists (decision D2): the playground would have nothing to call.
      playground: false,
      codeSamples: ["curl", "js", "python"],
    }),
  ],
  changelog: {
    title: { en: "Changelog", sk: "Zmeny" },
    description: { en: "What changed in each version of Accquix Register.", sk: "Čo sa zmenilo v každej verzii Accquix Register." },
  },
  seo: {
    og: {
      logo: "/logo-dark.svg",
      palette: { accent: "#3c9fff", background: "#0b0b0b", foreground: "#eeeeee", muted: "#b2b2b2", border: "#262626" },
    },
  },
  feedback: false,
  ai: { openInChat: false },
  deployment: { site: "https://register.accquix.com" },
});
