import eslint from "@eslint/js";
import tseslint from "typescript-eslint";
import reactPlugin from "eslint-plugin-react";

export default tseslint.config(
  eslint.configs.recommended,
  ...tseslint.configs.recommended,
  {
    plugins: {
      react: reactPlugin,
    },
    rules: {
      "complexity": ["error", 10]
    }
  },
  {
    // Node test suites exercise JSDOM browser globals alongside standard Node built-ins.
    files: ["app/**/*.test.mjs"],
    languageOptions: {
      globals: {
        AbortController: "readonly",
        AbortSignal: "readonly",
        DOMException: "readonly",
        File: "readonly",
        Response: "readonly",
        URL: "readonly",
        URLSearchParams: "readonly",
        document: "readonly",
        process: "readonly",
        sessionStorage: "readonly",
        setImmediate: "readonly",
        setTimeout: "readonly",
        structuredClone: "readonly",
        window: "readonly",
      },
    },
  }
);
