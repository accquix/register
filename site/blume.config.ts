import { defineConfig } from "blume";
import { openapi } from "blume/reference";

const inter = { name: "Inter", provider: "google" as const, subsets: ["latin", "latin-ext"], weights: [400, 500, 600, 700] };

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
      { label: { en: "Guides", sk: "Návody" }, path: "/" },
      { label: { en: "API reference", sk: "API referencia" }, path: "/reference" },
    ],
    repo: true,
  },
  github: { owner: "accquix", repo: "register", branch: "main", dir: "site/docs" },
  footer: {
    copyright: { en: "Code: Apache-2.0. Data: RPO © Ministerstvo vnútra SR, CC BY 4.0; Zdroj údajov: Finančná správa SR.", sk: "Kód: Apache-2.0. Dáta: RPO © Ministerstvo vnútra SR, CC BY 4.0; Zdroj údajov: Finančná správa SR." },
    links: [
      { label: "GitHub", href: "https://github.com/accquix/register" },
      { label: "Accquix", href: "https://accquix.com" },
    ],
  },
  theme: {
    accent: { light: "#0d74ce", dark: "#70b8ff" },
    radius: "md",
    mode: "system",
    toggle: true,
    fonts: { display: inter, body: inter, mono: "ibm-plex-mono" },
  },
  reference: [
    openapi({
      spec: "../api/openapi.yaml",
      // No public instance exists (decision D2): the playground would have nothing to call.
      playground: false,
      codeSamples: ["curl", "js", "python"],
    }),
  ],
  feedback: false,
  ai: { openInChat: false },
  deployment: { site: "https://register.accquix.com" },
});
