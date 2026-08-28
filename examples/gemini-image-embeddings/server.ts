import { timingSafeEqual } from "node:crypto"
import { DurableFileQueue } from "./queue"
import { HTTPError, ImageEmbeddingProcessor, type ObjectCreatedEvent, type ProcessorConfig } from "./processor"

const config: ProcessorConfig = {
  bucketMuxS3URL: required("BUCKETMUX_S3_URL"),
  bucketMuxAdminURL: required("BUCKETMUX_ADMIN_URL"),
  bucketMuxAccessKey: required("BUCKETMUX_S3_ACCESS_KEY"),
  bucketMuxSecretKey: required("BUCKETMUX_S3_SECRET_KEY"),
  bucketMuxAdminUser: required("BUCKETMUX_ADMIN_USER"),
  bucketMuxAdminPassword: required("BUCKETMUX_ADMIN_PASSWORD"),
  geminiAPIKey: required("GEMINI_API_KEY"),
  geminiBaseURL: process.env.GEMINI_BASE_URL,
  embeddingModel: process.env.GEMINI_EMBEDDING_MODEL || "gemini-embedding-2",
  outputDimensions: positiveInteger(process.env.GEMINI_OUTPUT_DIMENSIONS || "768", "GEMINI_OUTPUT_DIMENSIONS"),
}
const webhookSecret = required("BUCKETMUX_WEBHOOK_SECRET")
const processor = new ImageEmbeddingProcessor(config)
const queue = new DurableFileQueue(process.env.QUEUE_DIR || "./data/queue", async event => {
  try {
    await processor.process(event)
  } catch (error) {
    // A newer upload owns this key now. The newer generation has its own job,
    // so retrying this permanently stale result would only waste inference.
    if (error instanceof HTTPError && error.status === 409) {
      console.info("discarding superseded embedding job", event.bucket, event.key)
      return
    }
    throw error
  }
})
await queue.init()
await queue.drain()
setInterval(() => void queue.drain(), 1000)

const server = Bun.serve({
  port: Number(process.env.PORT || 8091),
  async fetch(request) {
    const url = new URL(request.url)
    if (request.method === "GET" && url.pathname === "/healthz") return Response.json({ ok: true })
    if (request.method !== "POST" || url.pathname !== "/hooks/object-created") return new Response("not found", { status: 404 })
    if (!secretMatches(request.headers.get("X-Webhook-Secret") || "", webhookSecret)) return new Response("unauthorized", { status: 401 })
    let event: ObjectCreatedEvent
    try {
      event = await request.json() as ObjectCreatedEvent
    } catch {
      return new Response("invalid JSON", { status: 400 })
    }
    if (event.event !== "object.created" || !event.bucket || !event.key || !supportedImage(event.contentType) || !event.checksumSHA256 || !event.objectUpdatedAt) {
      return new Response("unsupported or incomplete event", { status: 422 })
    }
    await queue.enqueue(event)
    void queue.drain()
    return Response.json({ queued: true }, { status: 202 })
  },
})

console.log(`Gemini image embedding worker listening on ${server.url}`)

function required(name: string): string {
  const value = process.env[name]?.trim()
  if (!value) throw new Error(`${name} is required`)
  return value
}

function secretMatches(received: string, expected: string): boolean {
  const left = Buffer.from(received)
  const right = Buffer.from(expected)
  return left.length === right.length && timingSafeEqual(left, right)
}

function positiveInteger(value: string, name: string): number {
  const parsed = Number(value)
  if (!Number.isInteger(parsed) || parsed < 128 || parsed > 3072) throw new Error(`${name} must be an integer between 128 and 3072`)
  return parsed
}

function supportedImage(contentType: string | undefined): boolean {
  return contentType === "image/jpeg" || contentType === "image/png"
}
