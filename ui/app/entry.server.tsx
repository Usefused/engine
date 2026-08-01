import fs from "node:fs";
import path from "node:path";
import type { EntryContext } from "@remix-run/node";
import { RemixServer } from "@remix-run/react";
import { renderToString } from "react-dom/server";

export default function handleRequest(
  request: Request,
  responseStatusCode: number,
  _responseHeaders: Headers,
  remixContext: EntryContext,
) {
  const shell = fs.readFileSync(
    path.join(process.cwd(), "app/index.html"),
    "utf8",
  );
  const app = renderToString(
    <RemixServer context={remixContext} url={request.url} />,
  );

  return new Response(shell.replace("<!-- Remix SPA -->", app), {
    headers: { "Content-Type": "text/html" },
    status: responseStatusCode,
  });
}
