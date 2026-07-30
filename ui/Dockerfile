FROM node:24-alpine AS builder

WORKDIR /app

# Upgrade base packages to fix vulnerabilities
RUN apk upgrade --no-cache

# Install dependencies
COPY package.json package-lock.json ./
RUN npm ci

# Copy source code (.dockerignore excludes node_modules so the Linux binaries from npm ci are preserved)
COPY . .

# Rebuild native binaries for the current platform (esbuild, etc.)
RUN npm rebuild && NODE_OPTIONS="--max-old-space-size=1536" npm run build

# Production image
FROM node:24-alpine AS runner

WORKDIR /app

# Upgrade base packages to fix vulnerabilities
RUN apk upgrade --no-cache

ENV NODE_ENV=production
ENV PORT=3000

# Install production dependencies only
COPY package.json package-lock.json ./
RUN npm ci --omit=dev

# Copy built SPA assets and the small static runtime server.
COPY --from=builder /app/build ./build
COPY --from=builder /app/server.mjs ./server.mjs

# Set non-root user for security
RUN addgroup -S remix && adduser -S remix -G remix
USER remix

EXPOSE 3000

CMD ["npm", "start"]
