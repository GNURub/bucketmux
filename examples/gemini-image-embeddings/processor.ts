export type ObjectCreatedEvent = {
  event: "object.created"
  bucket: string
  key: string
  contentType: string
  checksumSHA256: string
  objectUpdatedAt: string
}

export type ProcessorConfig = {
  bucketMuxS3URL: string
  bucketMuxAdminURL: string
  bucketMuxAccessKey: string
  bucketMuxSecretKey: string
  bucketMuxAdminUser: string
  bucketMuxAdminPassword: string
  geminiAPIKey: string
  geminiBaseURL?: string
  embeddingModel: string
  outputDimensions: number
}

type Fetcher = (input: string | URL | Request, init?: RequestInit) => Promise<Response>

export class HTTPError extends Error {
  constructor(readonly operation: string, readonly status: number, readonly detail: string) {
    super(`${operation} failed with HTTP ${status}: ${detail}`)
    this.name = "HTTPError"
  }
}

export class ImageEmbeddingProcessor {
  constructor(private readonly config: ProcessorConfig, private readonly fetcher: Fetcher = fetch) {}

  async process(event: ObjectCreatedEvent): Promise<{ dimensions: number }> {
    if (event.contentType !== "image/jpeg" && event.contentType !== "image/png") {
      throw new Error(`unsupported content type ${event.contentType}; Gemini image embeddings require image/jpeg or image/png`)
    }
    if (!event.checksumSHA256 || !event.objectUpdatedAt) throw new Error("hook payload is missing object generation fields")

    const image = await this.downloadObject(event)
    const vector = await this.embed([{ inline_data: { mime_type: event.contentType, data: Buffer.from(image).toString("base64") } }])
    await this.importEmbedding(event, vector)
    return { dimensions: vector.length }
  }

  private async downloadObject(event: ObjectCreatedEvent): Promise<ArrayBuffer> {
    const objectPath = [event.bucket, ...event.key.split("/")].map(encodeURIComponent).join("/")
    const response = await this.fetcher(`${trimSlash(this.config.bucketMuxS3URL)}/${objectPath}`, {
      headers: {
        "X-S3LS-Access-Key": this.config.bucketMuxAccessKey,
        "X-S3LS-Secret-Key": this.config.bucketMuxSecretKey,
      },
    })
    await requireOK(response, "download BucketMux object")
    return response.arrayBuffer()
  }

  async embedText(text: string): Promise<number[]> {
    if (!text.trim()) throw new Error("query text is required")
    return this.embed([{ text: text.trim() }])
  }

  private async embed(parts: Array<Record<string, unknown>>): Promise<number[]> {
    const baseURL = trimSlash(this.config.geminiBaseURL || "https://generativelanguage.googleapis.com/v1beta")
    const model = encodeURIComponent(this.config.embeddingModel)
    const response = await this.fetcher(`${baseURL}/models/${model}:embedContent`, {
      method: "POST",
      headers: { "Content-Type": "application/json", "x-goog-api-key": this.config.geminiAPIKey },
      body: JSON.stringify({
        content: { parts },
        output_dimensionality: this.config.outputDimensions,
      }),
    })
    await requireOK(response, "Gemini embedding response")
    const payload = await response.json() as { embedding?: { values?: number[] } }
    const vector = payload.embedding?.values
    if (!vector?.length || vector.some(value => !Number.isFinite(value))) throw new Error("Gemini embedding response did not contain a finite vector")
    if (vector.length !== this.config.outputDimensions) {
      throw new Error(`Gemini returned ${vector.length} dimensions; expected ${this.config.outputDimensions}`)
    }
    return vector
  }

  private async importEmbedding(event: ObjectCreatedEvent, vector: number[]): Promise<void> {
    const modelVersion = `${this.config.embeddingModel};dimensions=${this.config.outputDimensions}`
    const response = await this.fetcher(`${trimSlash(this.config.bucketMuxAdminURL)}/admin/api/embeddings`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Authorization: `Basic ${Buffer.from(`${this.config.bucketMuxAdminUser}:${this.config.bucketMuxAdminPassword}`).toString("base64")}`,
      },
      body: JSON.stringify({
        bucket: event.bucket,
        key: event.key,
        producer_id: "external:gemini-image-embedding",
        source_checksum: event.checksumSHA256,
        source_updated_at: event.objectUpdatedAt,
        embeddings: [{
          kind: "image-multimodal",
          model: this.config.embeddingModel,
          model_version: modelVersion,
          metric: "cosine",
          dimensions: vector.length,
          values: vector,
          metadata: { provider: "google-gemini", input_mime_type: event.contentType },
        }],
      }),
    })
    await requireOK(response, "import BucketMux embedding")
  }
}

async function requireOK(response: Response, operation: string): Promise<void> {
  if (response.ok) return
  const detail = (await response.text()).slice(0, 1000)
  throw new HTTPError(operation, response.status, detail)
}

function trimSlash(value: string): string {
  return value.replace(/\/+$/, "")
}
