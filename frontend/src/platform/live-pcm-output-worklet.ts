export const PCM_OUTPUT_PROCESSOR_NAME = 'term-llm-pcm-output';
export const PCM_OUTPUT_REPORT_QUANTA = 32;

export interface PCMOutputProcessorOptions {
  inputSampleRate: number;
  maxQueuedSamples: number;
  maxQueuedPackets: number;
  prebufferMS?: number;
  maxBufferWaitMS?: number;
}

export interface PCMOutputEnqueueMessage {
  type: 'enqueue';
  epoch: number;
  pcm: ArrayBuffer;
}

export interface PCMOutputResetMessage {
  type: 'reset';
  epoch: number;
}

export type PCMOutputCommand = PCMOutputEnqueueMessage | PCMOutputResetMessage;

export interface PCMOutputStateMessage {
  type: 'state';
  epoch: number;
  consumedBytes: number;
  consumedPackets: number;
  consumedSamples: number;
  queuedSamples: number;
  underruns: number;
}

export interface PCMOutputOverflowMessage {
  type: 'overflow';
  epoch: number;
}

export type PCMOutputEvent = PCMOutputStateMessage | PCMOutputOverflowMessage;

// This source is installed with a Blob URL because AudioWorkletGlobalScope cannot
// import the application's Vite chunk. Keep it self-contained and dependency-free.
export const pcmOutputProcessorSource = String.raw`
class TermLLMPCMOutput extends AudioWorkletProcessor {
  constructor(options) {
    super();
    const processorOptions = options.processorOptions || {};
    this.inputSampleRate = processorOptions.inputSampleRate || 24000;
    this.maxQueuedSamples = processorOptions.maxQueuedSamples || this.inputSampleRate * 60 * 10;
    this.maxQueuedPackets = processorOptions.maxQueuedPackets || 16384;
    this.step = this.inputSampleRate / sampleRate;
    this.prebufferSamples = Math.ceil(this.inputSampleRate * (processorOptions.prebufferMS ?? 150) / 1000);
    this.maxBufferWaitFrames = Math.ceil(sampleRate * (processorOptions.maxBufferWaitMS ?? 250) / 1000);
    this.buffering = true;
    this.bufferWaitFrames = 0;
    this.epoch = 0;
    this.chunks = [];
    this.headIndex = 0;
    this.headOffset = 0;
    this.queuedSamples = 0;
    this.queuedPackets = 0;
    this.queuedBytes = 0;
    this.consumedBytes = 0;
    this.consumedPackets = 0;
    this.consumedSamples = 0;
    this.phase = 0;
    this.hasStarted = false;
    this.inUnderrun = false;
    this.underruns = 0;
    this.dirty = false;
    this.reportCountdown = ${PCM_OUTPUT_REPORT_QUANTA};
    this.port.onmessage = (event) => this.onMessage(event.data);
  }

  onMessage(message) {
    if (!message || !Number.isSafeInteger(message.epoch)) return;
    if (message.type === 'reset') {
      if (message.epoch < this.epoch) return;
      this.reset(message.epoch);
      return;
    }
    if (message.type !== 'enqueue' || message.epoch !== this.epoch) return;
    const pcm = message.pcm;
    if (!(pcm instanceof ArrayBuffer) || pcm.byteLength === 0 || pcm.byteLength % 2 !== 0) return;
    const samples = pcm.byteLength / 2;
    if (
      this.queuedSamples + samples > this.maxQueuedSamples ||
      this.queuedPackets + 1 > this.maxQueuedPackets
    ) {
      this.port.postMessage({ type: 'overflow', epoch: this.epoch });
      return;
    }
    this.chunks.push(new Int16Array(pcm));
    this.queuedSamples += samples;
    this.queuedPackets += 1;
    this.queuedBytes += pcm.byteLength;
    this.dirty = true;
  }

  reset(epoch) {
    this.buffering = true;
    this.bufferWaitFrames = 0;
    this.epoch = epoch;
    this.chunks = [];
    this.headIndex = 0;
    this.headOffset = 0;
    this.queuedSamples = 0;
    this.queuedPackets = 0;
    this.queuedBytes = 0;
    this.consumedBytes = 0;
    this.consumedPackets = 0;
    this.consumedSamples = 0;
    this.phase = 0;
    this.hasStarted = false;
    this.inUnderrun = false;
    this.underruns = 0;
    this.dirty = false;
    this.reportCountdown = ${PCM_OUTPUT_REPORT_QUANTA};
  }

  sampleAt(offset) {
    const head = this.chunks[this.headIndex];
    const index = this.headOffset + offset;
    if (head && index < head.length) return head[index];
    const next = this.chunks[this.headIndex + 1];
    return next ? next[index - head.length] : 0;
  }

  consume(count) {
    let remaining = Math.min(count, this.queuedSamples);
    if (remaining > 0) {
      this.consumedSamples += remaining;
      this.dirty = true;
    }
    while (remaining > 0) {
      const head = this.chunks[this.headIndex];
      const available = head.length - this.headOffset;
      const consumed = Math.min(remaining, available);
      this.headOffset += consumed;
      this.queuedSamples -= consumed;
      remaining -= consumed;
      if (this.headOffset !== head.length) continue;
      this.chunks[this.headIndex] = null;
      this.headIndex += 1;
      this.headOffset = 0;
      this.queuedPackets -= 1;
      this.queuedBytes -= head.byteLength;
      this.consumedPackets += 1;
      this.consumedBytes += head.byteLength;
    }
    if (this.headIndex >= 1024 && this.headIndex * 2 >= this.chunks.length) {
      this.chunks = this.chunks.slice(this.headIndex);
      this.headIndex = 0;
    }
  }

  report() {
    this.port.postMessage({
      type: 'state',
      epoch: this.epoch,
      consumedBytes: this.consumedBytes,
      consumedPackets: this.consumedPackets,
      consumedSamples: this.consumedSamples,
      queuedSamples: this.queuedSamples,
      underruns: this.underruns,
    });
    this.dirty = false;
  }

  process(_inputs, outputs) {
    const output = outputs[0] && outputs[0][0];
    if (!output) return true;
    output.fill(0);
    let wrote = 0;
    for (; wrote < output.length; wrote += 1) {
      if (this.queuedSamples === 0) {
        this.phase = 0;
        this.buffering = true;
        this.bufferWaitFrames = 0;
        break;
      }
      // Measure waiting on the render clock, not main-thread timers. Idle
      // silence does not spend the deadline; it starts with the first sample.
      if (this.buffering) {
        if (this.queuedSamples < this.prebufferSamples && this.bufferWaitFrames < this.maxBufferWaitFrames) {
          this.bufferWaitFrames += 1;
          continue;
        }
        this.buffering = false;
        this.bufferWaitFrames = 0;
        this.inUnderrun = false;
      }
      const first = this.sampleAt(0);
      const second = this.queuedSamples > 1 ? this.sampleAt(1) : first;
      output[wrote] = (first + (second - first) * this.phase) / 32768;
      this.hasStarted = true;
      this.phase += this.step;
      const consumed = Math.floor(this.phase);
      if (consumed > 0) {
        this.consume(consumed);
        this.phase -= consumed;
      }
    }
    // Draining exactly on a quantum boundary still requires a refill, even
    // if the next packet reaches the port before the next process callback.
    if (this.queuedSamples === 0) {
      this.buffering = true;
      this.bufferWaitFrames = 0;
      this.phase = 0;
    }
    if (this.queuedSamples === 0 && this.hasStarted && !this.inUnderrun) {
      this.inUnderrun = true;
      this.underruns += 1;
      this.dirty = true;
    }
    this.reportCountdown -= 1;
    if (this.dirty && this.reportCountdown <= 0) {
      this.report();
      this.reportCountdown = ${PCM_OUTPUT_REPORT_QUANTA};
    }
    return true;
  }
}
registerProcessor('${PCM_OUTPUT_PROCESSOR_NAME}', TermLLMPCMOutput);
`;
