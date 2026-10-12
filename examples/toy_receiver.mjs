#!/usr/bin/env node
/** A tiny stand-in for the service you want to test. Standard library only.
 *
 *     node examples/toy_receiver.mjs
 *     node examples/toy_receiver.mjs --capacity 200 --drain 100 --api-key secret
 *
 * It listens on http://127.0.0.1:3000/ingest and accepts every payload shape
 * Signal Lab can send (batch, array, ndjson, single). Press Ctrl-C for a
 * summary. This is a teaching toy, not a model of a real service.
 */

import { createServer } from 'node:http'

const REQUIRED = ['event_id', 'device_id', 'event_time', 'temperature_c', 'vibration_mm_s']

function usage() {
	return `Usage: node examples/toy_receiver.mjs [options]

Options:
  --port <number>      port to listen on (default: 3000)
  --capacity <number> pretend queue size in events (default: 100000)
  --drain <number>    events per second the pretend queue empties at (default: 10000)
  --api-key <value>   require this X-Api-Key header
  --help              show this help`
}

function readNumber(name, raw, integer = false) {
	const value = Number(raw)
	if (!Number.isFinite(value) || (integer && !Number.isInteger(value))) {
		throw new Error(`${name} must be ${integer ? 'an integer' : 'a number'}`)
	}

	return value
}

function readArgs(argv) {
	const args = { port: 3000, capacity: 100000, drain: 10000, apiKey: '' }

	for (let index = 0; index < argv.length; index += 1) {
		const option = argv[index]
		if (option === '--help') {
			console.log(usage())
			process.exit(0)
		}

		const raw = argv[index + 1]
		if (raw === undefined) throw new Error(`${option} requires a value`)
		index += 1

		switch (option) {
			case '--port':
				args.port = readNumber(option, raw, true)
				break
			case '--capacity':
				args.capacity = readNumber(option, raw, true)
				break
			case '--drain':
				args.drain = readNumber(option, raw)
				break
			case '--api-key':
				args.apiKey = raw
				break
			default:
				throw new Error(`unknown option: ${option}`)
		}
	}

	return args
}

function parse(body, contentType) {
	const text = body.toString('utf8')
	if (contentType.includes('ndjson')) {
		return text
			.split(/\r?\n/u)
			.filter((line) => line.trim())
			.map((line) => JSON.parse(line))
	}

	const data = JSON.parse(text)
	if (data && !Array.isArray(data) && Array.isArray(data.events)) return data.events

	return Array.isArray(data) ? data : [data]
}

function problem(record) {
	if (!record || typeof record !== 'object' || Array.isArray(record)) {
		return 'a record is not a JSON object'
	}

	for (const field of REQUIRED) {
		if (!(field in record)) return `missing field ${field}`
	}

	for (const field of ['temperature_c', 'vibration_mm_s']) {
		if (typeof record[field] !== 'number') return `${field} must be a number`
	}

	return null
}

class State {
	constructor(capacity, drain) {
		this.capacity = capacity
		this.drain = drain
		this.level = 0
		this.updated = process.hrtime.bigint()
		this.seen = new Set()
		this.requests = 0
		this.events = 0
		this.duplicates = 0
		this.throttled = 0
		this.refused = 0
	}

	roomFor(count) {
		const now = process.hrtime.bigint()
		const elapsedSeconds = Number(now - this.updated) / 1e9
		this.level = Math.max(0, this.level - elapsedSeconds * this.drain)
		this.updated = now
		if (this.level + count > this.capacity) return false

		this.level += count

		return true
	}
}

function reply(response, status, payload, retryAfter = null) {
	const body = JSON.stringify(payload)
	const headers = {
		'Content-Type': 'application/json',
		'Content-Length': Buffer.byteLength(body),
	}
	if (retryAfter !== null) headers['Retry-After'] = String(retryAfter)
	response.writeHead(status, headers)
	response.end(body)
}

function makeServer(state, apiKey) {
	return createServer((request, response) => {
		if (request.method !== 'POST') {
			reply(response, 405, { error: 'method not allowed' })

			return
		}

		const chunks = []
		request.on('data', (chunk) => chunks.push(chunk))
		request.on('end', () => {
			if (apiKey && request.headers['x-api-key'] !== apiKey) {
				reply(response, 401, { error: 'missing or wrong X-Api-Key header' })

				return
			}

			let records
			try {
				records = parse(Buffer.concat(chunks), request.headers['content-type'] ?? '')
			} catch {
				reply(response, 400, { error: 'the body is not valid JSON' })

				return
			}

			const invalid = records.map(problem).find(Boolean)
			state.requests += 1
			if (invalid) {
				state.refused += 1
				reply(response, 400, { error: invalid })

				return
			}

			if (!state.roomFor(records.length)) {
				state.throttled += 1
				reply(response, 429, { error: 'too busy, try again' }, 1)

				return
			}

			let stored = 0
			for (const record of records) {
				if (state.seen.has(record.event_id)) {
					state.duplicates += 1
				} else {
					state.seen.add(record.event_id)
					stored += 1
				}
			}
			state.events += stored
			reply(response, 202, { accepted: records.length, stored })
		})
	})
}

let args
try {
	args = readArgs(process.argv.slice(2))
} catch (error) {
	console.error(`${error.message}\n\n${usage()}`)
	process.exit(2)
}

const state = new State(args.capacity, args.drain)
const server = makeServer(state, args.apiKey)
server.listen(args.port, '127.0.0.1', () => {
	console.log(`listening on http://127.0.0.1:${args.port}/ingest  (Ctrl-C for a summary)`)
})

process.on('SIGINT', () => {
	server.close(() => {
		console.log(
			`\n${state.requests} requests: ${state.events} events stored once, ` +
				`${state.duplicates} duplicates skipped, ${state.throttled} answered 429, ` +
				`${state.refused} refused as invalid`,
		)
		process.exit(0)
	})
})
