import { useEffect, useRef, useState, type CSSProperties } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import hostedStyles from "../../../../internal/engine/api/hosted_connect.css?raw";
import { safeLogoPreviewURL, safePrimaryColour } from "~/lib/connect-branding";

interface HostedConnectionPreviewProps {
  previewName: string;
  previewColour: string;
  previewLogoURL: string | null;
  supportURL: string;
  privacyURL: string;
}

// Match the hosted renderer's luminance threshold so bright brand colours retain readable labels.
export function previewAccentForeground(colour: string): string {
  const safe = safePrimaryColour(colour);
  // Linearize each sRGB channel before weighting its contribution to perceived brightness.
  const channels = [1, 3, 5].map((offset) => {
    const channel = parseInt(safe.slice(offset, offset + 2), 16) / 255;
    // The sRGB transfer curve uses a linear segment for low-intensity channels.
    return channel <= 0.04045 ? channel / 12.92 : ((channel + 0.055) / 1.055) ** 2.4;
  });
  const luminance = 0.2126 * channels[0] + 0.7152 * channels[1] + 0.0722 * channels[2];
  // This is the same contrast boundary used by hostedConnectBranding.AccentForeground.
  return luminance <= 0.1833 ? "#ffffff" : "#000000";
}

// Use escaped React markup and the actual hosted stylesheet inside an isolated, non-interactive document.
function previewDocument(props: HostedConnectionPreviewProps): string {
  const support = safeLogoPreviewURL(props.supportURL);
  const privacy = safeLogoPreviewURL(props.privacyURL);
  const style = {
    "--connect-accent": props.previewColour,
    "--connect-accent-foreground": previewAccentForeground(props.previewColour),
  } as CSSProperties;
  return "<!doctype html>" + renderToStaticMarkup(
    <html lang="en">
      <head>
        <meta charSet="utf-8" />
        <meta name="viewport" content="width=device-width,initial-scale=1" />
        <title>Connection page preview</title>
        <style>{hostedStyles}</style>
        {/* Only the surrounding page spacing changes to fit the embedded preview. */}
        <style>{"body{min-height:0;padding:16px}main{margin:16px auto}"}</style>
      </head>
      <body>
        <main style={style}>
          <header className="connect-brand">
            {/* The hosted page renders a logo only when one has been configured. */}
            {props.previewLogoURL && <img className="connect-logo" src={props.previewLogoURL} width="48" height="48" alt="" referrerPolicy="no-referrer" />}
            <span>{props.previewName}</span>
          </header>
          <p className="connect-eyebrow">Secure connection</p>
          <h1>Connect your account</h1>
          <p className="connect-copy">Enter the details needed to continue to your provider securely.</p>
          <div className="connect-form">
            <p className="connect-form-helper">These details select the correct provider account or site.</p>
            <div className="connect-field">
              <label className="connect-label" htmlFor="preview-account"><span>Account URL</span><span className="connect-field-requirement">Required</span></label>
              <input id="preview-account" placeholder="your-team.example.com" readOnly tabIndex={-1} />
              <p className="connect-field-description">Example field — actual fields depend on the service.</p>
            </div>
            <span className="connect-action">Continue</span>
          </div>
          {/* Optional footer entries appear only for valid configured destinations, without enabling navigation. */}
          {(support || privacy) && <footer className="connect-links">
            {/* Inert anchors preserve the real link styling without leaving Settings. */}
            {support && <a>Support</a>}
            {/* The privacy entry follows the same independent configuration as the hosted page. */}
            {privacy && <a>Privacy</a>}
          </footer>}
        </main>
      </body>
    </html>,
  );
}

// Size the preview to its document so narrow screens can show the real page without a nested scrollbar.
export function HostedConnectionPreview(props: HostedConnectionPreviewProps) {
  const [height, setHeight] = useState(640);
  const observer = useRef<ResizeObserver | null>(null);
  const document = previewDocument(props);
  // Disconnect observations when navigation removes the preview.
  useEffect(() => () => observer.current?.disconnect(), []);

  // Reattach measurement after draft edits replace the iframe document, including late logo loads.
  function measurePreview(event: React.SyntheticEvent<HTMLIFrameElement>) {
    observer.current?.disconnect();
    const body = event.currentTarget.contentDocument?.body;
    // A not-yet-readable frame keeps its initial reserved height until the next load.
    if (!body) return;
    // Include the frame border so its content fits without a two-pixel nested scrollbar.
    const measure = () => setHeight(Math.ceil(body.getBoundingClientRect().height) + 2);
    observer.current = new ResizeObserver(measure);
    observer.current.observe(body);
    measure();
  }

  return <section aria-label="Connection page preview">
    <p className="mb-2 text-sm font-medium text-slate-700">Live preview</p>
    <p className="mb-3 text-xs text-slate-500">Hosted connection page with an example field. Provider sign-in opens separately.</p>
    <iframe title="Connection page preview" srcDoc={document} sandbox="allow-same-origin" onLoad={measurePreview} style={{ height }} className="block w-full rounded-xl border border-slate-200" />
  </section>;
}
