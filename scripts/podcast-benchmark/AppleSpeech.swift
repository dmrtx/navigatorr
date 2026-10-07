// Experimental issue #93 benchmark. Not a worker or a scheduler.
import AVFoundation
import CryptoKit
import Foundation
import Speech

struct TimedUnit: Codable, Sendable {
    let id: String
    let start_ms: Int
    let end_ms: Int
    let text: String
    let timing: String
}

struct Transcript: Encodable {
    let schema_version = 1
    let source_hash: String
    let provider = "apple_speech"
    let provider_version: String
    let language: String
    let duration_ms: Int
    let wall_seconds: Double
    let realtime_factor: Double
    let timing_granularity = "native_attributed_run_or_result"
    let units: [TimedUnit]
}

enum BenchmarkError: Error, CustomStringConvertible {
    case message(String)
    var description: String {
        switch self { case .message(let value): return value }
    }
}

func fingerprint(_ path: String) throws -> String {
    let file = try FileHandle(forReadingFrom: URL(fileURLWithPath: path))
    defer { try? file.close() }
    var hash = SHA256()
    while let data = try file.read(upToCount: 1_048_576), !data.isEmpty {
        hash.update(data: data)
    }
    return "sha256:" + hash.finalize().map { String(format: "%02x", $0) }.joined()
}

func milliseconds(_ time: CMTime) throws -> Int {
    let seconds = CMTimeGetSeconds(time)
    guard seconds.isFinite, seconds >= 0 else {
        throw BenchmarkError.message("ASR returned an invalid audio timestamp")
    }
    return Int((seconds * 1000).rounded())
}

func emit<T: Encodable>(_ value: T, to path: String? = nil) throws {
    let encoder = JSONEncoder()
    encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
    let data = try encoder.encode(value)
    if let path {
        try data.write(to: URL(fileURLWithPath: path), options: .atomic)
    } else {
        FileHandle.standardOutput.write(data)
        FileHandle.standardOutput.write(Data("\n".utf8))
    }
}

@main
struct AppleSpeechBenchmark {
    static func main() async {
        do { try await run() }
        catch {
            FileHandle.standardError.write(Data("apple_speech: \(error)\n".utf8))
            exit(1)
        }
    }

    static func run() async throws {
        guard #available(macOS 26, *) else {
            throw BenchmarkError.message("SpeechAnalyzer requires macOS 26 or later")
        }
        let args = Array(CommandLine.arguments.dropFirst())
        guard args == ["probe"] || args.count == 3 || args.count == 4 else {
            throw BenchmarkError.message("usage: apple-speech probe | AUDIO TRANSCRIPT.json LANGUAGE [--install-assets]")
        }
        let available = SpeechTranscriber.isAvailable
        if args == ["probe"] {
            struct Probe: Encodable {
                let available: Bool
                let os_version: String
                let supported_locales: [String]
                let installed_locales: [String]
            }
            try await emit(Probe(
                available: available,
                os_version: ProcessInfo.processInfo.operatingSystemVersionString,
                supported_locales: SpeechTranscriber.supportedLocales.map(\.identifier),
                installed_locales: SpeechTranscriber.installedLocales.map(\.identifier)
            ))
            return
        }
        guard available else { throw BenchmarkError.message("SpeechTranscriber unavailable on this host") }
        guard let locale = await SpeechTranscriber.supportedLocale(equivalentTo: Locale(identifier: args[2])) else {
            throw BenchmarkError.message("Requested language is unsupported: \(args[2])")
        }
        if args.count == 4 && args[3] != "--install-assets" {
            throw BenchmarkError.message("Unknown option: \(args[3])")
        }
        let transcriber = SpeechTranscriber(
            locale: locale, transcriptionOptions: [], reportingOptions: [],
            attributeOptions: [.audioTimeRange]
        )
        if await AssetInventory.status(forModules: [transcriber]) != .installed {
            guard args.last == "--install-assets" else {
                throw BenchmarkError.message("Language assets are not installed; use --install-assets explicitly")
            }
            if let request = try await AssetInventory.assetInstallationRequest(supporting: [transcriber]) {
                try await request.downloadAndInstall()
            }
        }
        let sourceHash = try fingerprint(args[0])
        let audio = try AVAudioFile(forReading: URL(fileURLWithPath: args[0]))
        let duration = Double(audio.length) / audio.processingFormat.sampleRate
        guard duration.isFinite, duration > 0 else { throw BenchmarkError.message("Empty/invalid audio") }
        let analyzer = SpeechAnalyzer(modules: [transcriber])
        let started = ContinuousClock.now
        let collector = Task { () throws -> [TimedUnit] in
            var units: [TimedUnit] = []
            for try await result in transcriber.results {
                var emitted = false
                for run in result.text.runs {
                    let text = String(result.text[run.range].characters).trimmingCharacters(in: .whitespacesAndNewlines)
                    guard !text.isEmpty else { continue }
                    guard let range = run.audioTimeRange else { continue }
                    let start = try milliseconds(range.start)
                    let end = try milliseconds(CMTimeRangeGetEnd(range))
                    guard end > start else { throw BenchmarkError.message("ASR produced a zero-duration unit") }
                    units.append(TimedUnit(id: String(format: "u%06d", units.count + 1), start_ms: start,
                                           end_ms: end, text: text, timing: "native_attributed_run"))
                    emitted = true
                }
                if !emitted {
                    let text = String(result.text.characters).trimmingCharacters(in: .whitespacesAndNewlines)
                    if !text.isEmpty {
                        let start = try milliseconds(result.range.start)
                        let end = try milliseconds(CMTimeRangeGetEnd(result.range))
                        guard end > start else { throw BenchmarkError.message("ASR produced a zero-duration result") }
                        units.append(TimedUnit(id: String(format: "u%06d", units.count + 1), start_ms: start,
                                               end_ms: end, text: text, timing: "native_result"))
                    }
                } else {
                    // Never silently drop untimed text from a partially attributed result.
                    let all = String(result.text.characters).split(whereSeparator: \.isWhitespace).joined(separator: " ")
                    let timed = result.text.runs.compactMap { run -> String? in
                        guard run.audioTimeRange != nil else { return nil }
                        return String(result.text[run.range].characters)
                    }.joined().split(whereSeparator: \.isWhitespace).joined(separator: " ")
                    guard all == timed else { throw BenchmarkError.message("ASR returned partially untimed text") }
                }
            }
            return units
        }
        do {
            _ = try await analyzer.analyzeSequence(from: audio)
            try await analyzer.finalizeAndFinishThroughEndOfInput()
            let units = try await collector.value
            let elapsed = started.duration(to: .now)
            let wall = Double(elapsed.components.seconds) + Double(elapsed.components.attoseconds) / 1e18
            guard !units.isEmpty else { throw BenchmarkError.message("ASR completed without any timed text") }
            guard try fingerprint(args[0]) == sourceHash else { throw BenchmarkError.message("Source changed during ASR") }
            let transcript = Transcript(source_hash: sourceHash,
                provider_version: ProcessInfo.processInfo.operatingSystemVersionString,
                language: locale.identifier, duration_ms: Int((duration * 1000).rounded()),
                wall_seconds: wall, realtime_factor: wall / duration, units: units)
            try emit(transcript, to: args[1])
            print("transcribed \(units.count) native timed units in \(wall)s; RTF \(wall / duration)")
        } catch {
            await analyzer.cancelAndFinishNow()
            collector.cancel()
            throw error
        }
    }
}
