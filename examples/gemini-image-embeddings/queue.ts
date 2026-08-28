import { createHash, randomUUID } from "node:crypto"
import { link, mkdir, readdir, readFile, rename, unlink, writeFile } from "node:fs/promises"
import { join } from "node:path"
import type { ObjectCreatedEvent } from "./processor"

type QueueJob = { event: ObjectCreatedEvent; attempts: number; nextAttemptAt: number; lastError?: string }

export class DurableFileQueue {
  private draining = false

  constructor(
    private readonly directory: string,
    private readonly processEvent: (event: ObjectCreatedEvent) => Promise<unknown>,
    private readonly maxAttempts = 8,
  ) {}

  async init(): Promise<void> {
    await mkdir(this.directory, { recursive: true })
  }

  async enqueue(event: ObjectCreatedEvent): Promise<void> {
    const identity = `${event.bucket}\0${event.key}\0${event.checksumSHA256}\0${event.objectUpdatedAt}`
    const id = createHash("sha256").update(identity).digest("hex")
    await this.writeNewAtomic(`${id}.json`, { event, attempts: 0, nextAttemptAt: Date.now() })
  }

  async drain(): Promise<void> {
    if (this.draining) return
    this.draining = true
    try {
      const files = (await readdir(this.directory)).filter(name => name.endsWith(".json") && !name.endsWith(".failed.json")).sort()
      for (const file of files) {
        const path = join(this.directory, file)
        let job: QueueJob
        try {
          job = JSON.parse(await readFile(path, "utf8")) as QueueJob
        } catch (error) {
          await rename(path, `${path}.failed`)
          console.error("invalid queue job", file, error)
          continue
        }
        if (job.nextAttemptAt > Date.now()) continue
        try {
          await this.processEvent(job.event)
          await unlink(path)
        } catch (error) {
          job.attempts++
          job.lastError = error instanceof Error ? error.message : String(error)
          if (job.attempts >= this.maxAttempts) {
            await this.writeAtomic(file, job)
            await rename(path, path.replace(/\.json$/, ".failed.json"))
            console.error("embedding job exhausted retries", job.event.bucket, job.event.key, job.lastError)
            continue
          }
          job.nextAttemptAt = Date.now() + Math.min(60_000, 1000 * 2 ** job.attempts)
          await this.writeAtomic(file, job)
        }
      }
    } finally {
      this.draining = false
    }
  }

  private async writeAtomic(file: string, job: QueueJob): Promise<void> {
    const destination = join(this.directory, file)
    const temporary = `${destination}.${randomUUID()}.tmp`
    await writeFile(temporary, JSON.stringify(job))
    await rename(temporary, destination)
  }

  private async writeNewAtomic(file: string, job: QueueJob): Promise<void> {
    const destination = join(this.directory, file)
    const temporary = `${destination}.${randomUUID()}.tmp`
    await writeFile(temporary, JSON.stringify(job))
    try {
      // A hard link publishes the complete temp file without replacing an
      // existing retry job for the same generation.
      await link(temporary, destination)
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "EEXIST") throw error
    } finally {
      await unlink(temporary).catch(() => {})
    }
  }
}
