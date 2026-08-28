const text = process.argv.slice(2).join(" ").trim()
if (!text) throw new Error("usage: bun run query -- <semantic query>")

const geminiBaseURL = trimSlash(process.env.GEMINI_BASE_URL || "https://generativelanguage.googleapis.com/v1beta")
const embeddingModel = process.env.GEMINI_EMBEDDING_MODEL || "gemini-embedding-2"
const outputDimensions = positiveInteger(process.env.GEMINI_OUTPUT_DIMENSIONS || "768", "GEMINI_OUTPUT_DIMENSIONS")
const geminiKey = required("GEMINI_API_KEY")
const adminURL = trimSlash(required("BUCKETMUX_ADMIN_URL"))
const adminAuth = Buffer.from(`${required("BUCKETMUX_ADMIN_USER")}:${required("BUCKETMUX_ADMIN_PASSWORD")}`).toString("base64")

const embeddingResponse = await fetch(`${geminiBaseURL}/models/${encodeURIComponent(embeddingModel)}:embedContent`, {
  method: "POST",
  headers: { "Content-Type": "application/json", "x-goog-api-key": geminiKey },
  body: JSON.stringify({ content: { parts: [{ text }] }, output_dimensionality: outputDimensions }),
})
if (!embeddingResponse.ok) throw new Error(`Gemini embedding failed: ${embeddingResponse.status} ${await embeddingResponse.text()}`)
const embeddingPayload = await embeddingResponse.json() as { embedding?: { values?: number[] } }
const vector = embeddingPayload.embedding?.values
if (!vector || vector.length !== outputDimensions) throw new Error(`Gemini returned ${vector?.length || 0} dimensions; expected ${outputDimensions}`)

const searchResponse = await fetch(`${adminURL}/admin/api/embeddings/search`, {
  method: "POST",
  headers: { "Content-Type": "application/json", Authorization: `Basic ${adminAuth}` },
  body: JSON.stringify({
    kind: "image-multimodal",
    model: embeddingModel,
    model_version: `${embeddingModel};dimensions=${outputDimensions}`,
    metric: "cosine",
    values: vector,
    limit: 10,
  }),
})
if (!searchResponse.ok) throw new Error(`BucketMux search failed: ${searchResponse.status} ${await searchResponse.text()}`)
console.log(JSON.stringify(await searchResponse.json(), null, 2))

function required(name: string): string {
  const value = process.env[name]?.trim()
  if (!value) throw new Error(`${name} is required`)
  return value
}

function trimSlash(value: string): string {
  return value.replace(/\/+$/, "")
}

function positiveInteger(value: string, name: string): number {
  const parsed = Number(value)
  if (!Number.isInteger(parsed) || parsed < 128 || parsed > 3072) throw new Error(`${name} must be an integer between 128 and 3072`)
  return parsed
}
