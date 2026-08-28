import { expect, test } from "bun:test"
import { mkdtemp, readdir, readFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { DurableFileQueue } from "./queue"
import type { ObjectCreatedEvent } from "./processor"

test("durable queue deduplicates a generation and removes it after success", async () => {
  const directory = await mkdtemp(join(tmpdir(), "bucketmux-gemini-queue-"))
  const processed: ObjectCreatedEvent[] = []
  const queue = new DurableFileQueue(directory, async event => { processed.push(event) })
  await queue.init()
  const event: ObjectCreatedEvent = {
    event: "object.created", bucket: "photos", key: "a.jpg", contentType: "image/jpeg",
    checksumSHA256: "checksum", objectUpdatedAt: "2026-08-28T10:00:00Z",
  }
  await queue.enqueue(event)
  await queue.enqueue(event)
  expect((await readdir(directory)).filter(name => name.endsWith(".json"))).toHaveLength(1)
  await queue.drain()
  expect(processed).toEqual([event])
  expect(await readdir(directory)).toHaveLength(0)
})

test("duplicate delivery does not reset retry state", async () => {
  const directory = await mkdtemp(join(tmpdir(), "bucketmux-gemini-retry-"))
  const queue = new DurableFileQueue(directory, async () => { throw new Error("temporary failure") })
  await queue.init()
  const event: ObjectCreatedEvent = {
    event: "object.created", bucket: "photos", key: "retry.jpg", contentType: "image/jpeg",
    checksumSHA256: "checksum", objectUpdatedAt: "2026-08-28T10:00:00Z",
  }
  await queue.enqueue(event)
  await queue.drain()
  const [file] = (await readdir(directory)).filter(name => name.endsWith(".json"))
  expect(JSON.parse(await readFile(join(directory, file), "utf8")).attempts).toBe(1)
  await queue.enqueue(event)
  expect(JSON.parse(await readFile(join(directory, file), "utf8")).attempts).toBe(1)
})
