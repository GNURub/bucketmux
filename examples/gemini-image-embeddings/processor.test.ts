import { describe, expect, test } from "bun:test"
import { ImageEmbeddingProcessor, type ObjectCreatedEvent, type ProcessorConfig } from "./processor"

const config: ProcessorConfig = {
  bucketMuxS3URL: "http://bucketmux:8080",
  bucketMuxAdminURL: "http://bucketmux:8080",
  bucketMuxAccessKey: "s3-access",
  bucketMuxSecretKey: "s3-secret",
  bucketMuxAdminUser: "admin",
  bucketMuxAdminPassword: "admin-secret",
  geminiAPIKey: "gemini-secret",
  geminiBaseURL: "http://gemini.test/v1beta",
  embeddingModel: "gemini-embedding-test",
  outputDimensions: 3,
}

const event: ObjectCreatedEvent = {
  event: "object.created",
  bucket: "photos",
  key: "incoming/cat photo.jpg",
  contentType: "image/jpeg",
  checksumSHA256: "abc123",
  objectUpdatedAt: "2026-08-28T10:00:00.123Z",
}

describe("Gemini image embedding processor", () => {
  test("downloads, embeds the image directly, and imports the current generation", async () => {
    const requests: Array<{ url: string; init?: RequestInit }> = []
    const responses = [
      new Response(new Uint8Array([0xff, 0xd8, 0xff]), { status: 200, headers: { "Content-Type": "image/jpeg" } }),
      Response.json({ embedding: { values: [0.5, 0.25, -0.75] } }),
      Response.json([{ model: "gemini-embedding-test", dimensions: 3 }], { status: 201 }),
    ]
    const fetcher = async (input: string | URL | Request, init?: RequestInit): Promise<Response> => {
      requests.push({ url: String(input), init })
      const response = responses.shift()
      if (!response) throw new Error("unexpected request")
      return response
    }

    const result = await new ImageEmbeddingProcessor(config, fetcher).process(event)
    expect(result).toEqual({ dimensions: 3 })
    expect(requests.map(request => request.url)).toEqual([
      "http://bucketmux:8080/photos/incoming/cat%20photo.jpg",
      "http://gemini.test/v1beta/models/gemini-embedding-test:embedContent",
      "http://bucketmux:8080/admin/api/embeddings",
    ])
    const geminiBody = JSON.parse(String(requests[1].init?.body))
    expect(geminiBody.content.parts[0].inline_data.mime_type).toBe("image/jpeg")
    expect(geminiBody.content.parts[0].inline_data.data).toBe("/9j/")
    expect(geminiBody.output_dimensionality).toBe(3)
    expect(requests[1].init?.headers).toMatchObject({ "x-goog-api-key": "gemini-secret" })
    const importBody = JSON.parse(String(requests[2].init?.body))
    expect(importBody.source_checksum).toBe(event.checksumSHA256)
    expect(importBody.source_updated_at).toBe(event.objectUpdatedAt)
    expect(importBody.embeddings[0]).toMatchObject({
      kind: "image-multimodal",
      model: "gemini-embedding-test",
      model_version: "gemini-embedding-test;dimensions=3",
      metric: "cosine",
      dimensions: 3,
      values: [0.5, 0.25, -0.75],
    })
  })

  test("propagates a stale-generation conflict without swallowing it", async () => {
    const responses = [
      new Response(new Uint8Array([1]), { status: 200 }),
      Response.json({ embedding: { values: [1, 0, 0] } }),
      new Response('{"type":"embedding-source-superseded"}', { status: 409 }),
    ]
    const fetcher = async (): Promise<Response> => responses.shift()!
    await expect(new ImageEmbeddingProcessor(config, fetcher).process(event)).rejects.toThrow("HTTP 409")
  })

  test("uses the same multimodal model for a text query", async () => {
    let body: any
    const fetcher = async (_input: string | URL | Request, init?: RequestInit): Promise<Response> => {
      body = JSON.parse(String(init?.body))
      return Response.json({ embedding: { values: [1, 0, 0] } })
    }
    const vector = await new ImageEmbeddingProcessor(config, fetcher).embedText("red bicycle")
    expect(vector).toEqual([1, 0, 0])
    expect(body).toEqual({ content: { parts: [{ text: "red bicycle" }] }, output_dimensionality: 3 })
  })
})
