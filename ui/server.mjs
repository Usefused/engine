import { createReadStream } from "node:fs";
import { stat } from "node:fs/promises";
import { createServer } from "node:http";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createGzip } from "node:zlib";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const clientDir = path.resolve(__dirname, "build", "client");
const clientDirPrefix = clientDir.endsWith(path.sep) ? clientDir : `${clientDir}${path.sep}`;
const port = Number(process.env.PORT || 3000);
const backendURL = process.env.BACKEND_URL || "http://localhost:8081";
const enginePublicURL = process.env.FUSED_ENGINE_PUBLIC_URL || "";
const enginePublicGRPCURL = process.env.FUSED_ENGINE_PUBLIC_GRPC_URL || "";

const contentTypes = {
	".css": "text/css; charset=utf-8",
	".html": "text/html; charset=utf-8",
	".js": "text/javascript; charset=utf-8",
	".json": "application/json; charset=utf-8",
	".svg": "image/svg+xml",
	".txt": "text/plain; charset=utf-8",
};

// Handle incoming SPA static asset and fallback navigation requests safely.
const server = createServer(async (req, res) => {
	// Serve runtime environment configuration directly for standalone UI hosting.
	if (req.url === "/env.js") {
		serveEnv(res);
		return;
	}

	// Reject mutating methods since the static server only serves read-only assets.
	if (req.method !== "GET" && req.method !== "HEAD") {
		res.writeHead(405);
		res.end();
		return;
	}

	// Reject malformed Host headers or request targets instead of crashing the async handler.
	let url;
	try {
		url = new URL(req.url || "/", `http://${req.headers.host || "localhost"}`);
	} catch {
		res.writeHead(400);
		res.end("Bad request");
		return;
	}

	const assetPath = resolveAssetPath(url.pathname);
	// Return matched static assets directly before falling back to SPA index routing.
	if (assetPath && (await serveFile(res, req, assetPath))) {
		return;
	}

	// Route browser HTML navigations to the SPA entry point so client routing takes over.
	if (isNavigation(req)) {
		await serveFile(res, req, path.join(clientDir, "index.html"));
		return;
	}

	res.writeHead(404);
	res.end("Not found");
});

server.listen(port, () => {
	console.log(`Fused UI SPA listening on http://localhost:${port}`);
});

// serveEnv exposes non-secret deployment choices for standalone UI hosting.
function serveEnv(res) {
	// External/static UI runs need an absolute API origin; embedded Engine UI
	// uses root.tsx's default relative origin and does not depend on this file.
	res.writeHead(200, {
		"Cache-Control": "no-cache",
		"Content-Type": "text/javascript; charset=utf-8",
		"Vary": "Accept-Encoding",
	});
	res.end(
		`window.__FUSED_ENV=${JSON.stringify({
			BACKEND_URL: backendURL,
			IMPORTED_MCP_ENABLED: process.env.FUSED_ENABLE_IMPORTED_MCP === "true",
			ENGINE_PUBLIC_URL: enginePublicURL,
			ENGINE_PUBLIC_GRPC_URL: enginePublicGRPCURL,
		})};`
	);
}

// resolveAssetPath maps a URL pathname to a file strictly inside clientDir.
function resolveAssetPath(rawPath) {
	// Reject percent-encoding errors or null bytes before normalizing filesystem paths.
	let decodedPath;
	try {
		decodedPath = decodeURIComponent(rawPath);
	} catch {
		return null;
	}
	if (decodedPath.includes("\0")) return null;

	const cleanPath = path.posix.normalize(`/${decodedPath}`).replace(/^\/+/, "");
	// Empty or root paths fall through to SPA navigation handling rather than directory reads.
	if (!cleanPath || cleanPath === ".") return null;
	// Guard against POSIX traversal prefixes before resolving against the host filesystem.
	if (cleanPath.startsWith("..")) return null;

	const resolvedPath = path.resolve(clientDir, cleanPath);
	// Enforce absolute path containment inside clientDir even when path separators differ by OS.
	if (!resolvedPath.startsWith(clientDirPrefix)) return null;
	return resolvedPath;
}

async function serveFile(res, req, filePath) {
	const info = await fileInfo(filePath);
	if (!info) return false;
	const type = contentType(filePath);
	const gzip = acceptsGzip(req) && shouldCompress(type);

	res.writeHead(200, {
		"Cache-Control": cacheControl(filePath),
		...(gzip ? { "Content-Encoding": "gzip", "Vary": "Accept-Encoding" } : { "Content-Length": info.size }),
		"Content-Type": type,
		"Last-Modified": info.mtime.toUTCString(),
	});
	if (req.method !== "HEAD") {
		const stream = createReadStream(filePath);
		if (gzip) {
			stream.pipe(createGzip()).pipe(res);
		} else {
			stream.pipe(res);
		}
	} else {
		res.end();
	}
	return true;
}

async function fileInfo(filePath) {
	try {
		const info = await stat(filePath);
		return info.isFile() ? info : null;
	} catch {
		return null;
	}
}

function isNavigation(req) {
	return (req.headers.accept || "").toLowerCase().includes("text/html");
}

function contentType(filePath) {
	return contentTypes[path.extname(filePath)] || "application/octet-stream";
}

function acceptsGzip(req) {
	return (req.headers["accept-encoding"] || "").includes("gzip");
}

function shouldCompress(type) {
	return type.startsWith("text/") || type.includes("javascript") || type.includes("json") || type.includes("svg");
}

function cacheControl(filePath) {
	const name = path.basename(filePath);
	if (name === "index.html" || name === "notification-service-worker.js") {
		return "no-cache";
	}
	if (filePath.includes(`${path.sep}assets${path.sep}`)) {
		return "public, max-age=31536000, immutable";
	}
	return "public, max-age=3600";
}
