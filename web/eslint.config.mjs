import nextTs from "eslint-config-next/typescript";
import nextVitals from "eslint-config-next/core-web-vitals";

const eslintConfig = [
  {
    ignores: [
      "node_modules/**",
      ".next/**",
      ".open-next/**",
      ".wrangler/**",
      "out/**",
      "build/**",
      "dist/**",
      "coverage/**",
      "next-env.d.ts",
    ],
  },
  ...nextVitals,
  ...nextTs,
  {
    rules: {
      // Bilingual CJK content uses curly quotes intentionally
      "react/no-unescaped-entities": "off",
    },
  },
];

export default eslintConfig;
