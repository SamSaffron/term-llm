import { describe, expect, it } from 'vitest';
import {
  PCM_OUTPUT_PROCESSOR_NAME,
  PCM_OUTPUT_REPORT_QUANTA,
  pcmOutputProcessorSource,
  type PCMOutputCommand,
  type PCMOutputEvent,
  type PCMOutputProcessorOptions,
} from './live-pcm-output-worklet';

interface TestProcessor {
  port: TestPort;
  process(inputs: Float32Array[][], outputs: Float32Array[][]): boolean;
}

type ProcessorConstructor = new (options: {
  processorOptions: PCMOutputProcessorOptions;
}) => TestProcessor;

class TestPort {
  onmessage: ((event: MessageEvent<PCMOutputCommand>) => void) | null = null;
  readonly messages: PCMOutputEvent[] = [];

  postMessage(message: PCMOutputEvent): void {
    this.messages.push(message);
  }

  send(message: PCMOutputCommand): void {
    this.onmessage?.({ data: message } as MessageEvent<PCMOutputCommand>);
  }
}

function createProcessor(
  outputSampleRate: number,
  overrides: Partial<PCMOutputProcessorOptions> = {},
): TestProcessor {
  let Processor: ProcessorConstructor | undefined;
  class TestAudioWorkletProcessor {
    readonly port = new TestPort();
  }
  new Function(
    'AudioWorkletProcessor',
    'sampleRate',
    'registerProcessor',
    pcmOutputProcessorSource,
  )(
    TestAudioWorkletProcessor,
    outputSampleRate,
    (name: string, constructor: ProcessorConstructor) => {
      expect(name).toBe(PCM_OUTPUT_PROCESSOR_NAME);
      Processor = constructor;
    },
  );
  if (!Processor) throw new Error('PCM output processor was not registered');
  return new Processor({
    processorOptions: {
      inputSampleRate: 24_000,
      maxQueuedSamples: 24_000 * 60 * 10,
      maxQueuedPackets: 16_384,
      prebufferMS: 0, // Isolate waveform tests from startup latency.
      ...overrides,
    },
  });
}

function enqueue(processor: TestProcessor, epoch: number, values: number[]): void {
  processor.port.send({ type: 'enqueue', epoch, pcm: new Int16Array(values).buffer });
}

function render(processor: TestProcessor, frames: number): Float32Array {
  const output = new Float32Array(frames);
  expect(processor.process([], [[output]])).toBe(true);
  return output;
}

function normalized(sample: number): number {
  return sample / 32768;
}

describe('PCM output AudioWorklet', () => {
  it.each([24_000, 44_100, 48_000])(
    'buffers 150 ms of PCM and starts immediately at the threshold (%i Hz)',
    (rate) => {
      const processor = createProcessor(rate, { prebufferMS: undefined });
      enqueue(processor, 0, new Array(2400).fill(8192));
      expect(render(processor, Math.floor(rate * 0.1)).every((sample) => sample === 0)).toBe(true);
      enqueue(processor, 0, new Array(1200).fill(16384));
      expect(render(processor, 1)[0]).toBe(0.25);
    },
  );

  it.each([24_000, 44_100, 48_000])(
    'releases a short response after at most 250 ms from arrival (%i Hz)',
    (rate) => {
      const processor = createProcessor(rate, { prebufferMS: undefined });
      // Silence before arrival must not use up the waiting budget.
      render(processor, rate);
      enqueue(processor, 0, [8192]);
      const wait = Math.ceil(rate * 0.25);
      expect(render(processor, wait).every((sample) => sample === 0)).toBe(true);
      expect(render(processor, 1)[0]).toBe(0.25);
    },
  );

  it('does not restart the deadline on each small arriving packet', () => {
    const processor = createProcessor(24_000, { prebufferMS: undefined });
    enqueue(processor, 0, [8192]);
    render(processor, 2400);
    enqueue(processor, 0, [16384]);
    render(processor, 2400);
    enqueue(processor, 0, [24576]);
    expect(render(processor, 1200).every((sample) => sample === 0)).toBe(true);
    expect([...render(processor, 3)]).toEqual([0.25, 0.5, 0.75]);
  });

  it('refills after starvation without losing samples or counting every waiting quantum as an underrun', () => {
    const processor = createProcessor(24_000, { prebufferMS: undefined });
    enqueue(processor, 0, new Array(3600).fill(8192));
    const first = render(processor, 3601);
    expect(first.slice(0, 3600).every((sample) => sample === 0.25)).toBe(true);
    expect(first[3600]).toBe(0);
    enqueue(processor, 0, new Array(1200).fill(16384));
    for (let i = 0; i < PCM_OUTPUT_REPORT_QUANTA; i += 1)
      expect(render(processor, 128).every((sample) => sample === 0)).toBe(true);
    expect(processor.port.messages.at(-1)).toMatchObject({
      underruns: 1,
      consumedPackets: 1,
      queuedSamples: 1200,
    });
    enqueue(processor, 0, new Array(2400).fill(24576));
    expect([...render(processor, 3600)]).toEqual([
      ...new Array(1200).fill(0.5),
      ...new Array(2400).fill(0.75),
    ]);
  });

  it('refills when audio drains exactly at the end of a render block', () => {
    const processor = createProcessor(24_000, { prebufferMS: undefined });
    enqueue(processor, 0, new Array(3600).fill(8192));
    render(processor, 3600);
    enqueue(processor, 0, [16384]);
    expect(render(processor, 6000).every((sample) => sample === 0)).toBe(true);
    expect(render(processor, 1)[0]).toBe(0.5);
  });

  it('interrupts buffered speech immediately and gives the next epoch a fresh deadline', () => {
    const processor = createProcessor(24_000, { prebufferMS: undefined });
    enqueue(processor, 0, [8192]);
    render(processor, 5900);
    processor.port.send({ type: 'reset', epoch: 1 });
    enqueue(processor, 0, [32767]);
    enqueue(processor, 1, [-8192]);
    expect(render(processor, 6000).every((sample) => sample === 0)).toBe(true);
    expect(render(processor, 1)[0]).toBe(-0.25);
  });

  it('renders one exact continuous 24 kHz signal across packet boundaries', () => {
    const processor = createProcessor(24_000);
    processor.port.send({ type: 'reset', epoch: 1 });
    enqueue(processor, 1, [0, 8_192, 16_384]);
    enqueue(processor, 1, [24_576, 32_767, -32_768]);

    expect([...render(processor, 6)]).toEqual(
      [0, 8_192, 16_384, 24_576, 32_767, -32_768].map(normalized),
    );
  });

  it.each([48_000, 44_100])(
    'keeps linear interpolation phase continuous across packets at %i Hz',
    (outputRate) => {
      const processor = createProcessor(outputRate);
      processor.port.send({ type: 'reset', epoch: 7 });
      const signal = [0, 12_000, 24_000, 6_000, -12_000, -24_000];
      enqueue(processor, 7, signal.slice(0, 3));
      enqueue(processor, 7, signal.slice(3));

      const step = 24_000 / outputRate;
      const expected: number[] = [];
      for (let position = 0; position < signal.length - 1; position += step) {
        const index = Math.floor(position);
        const fraction = position - index;
        expected.push(normalized(signal[index] + (signal[index + 1] - signal[index]) * fraction));
      }
      const rendered = render(processor, expected.length);

      expected.forEach((sample, index) => expect(rendered[index]).toBeCloseTo(sample, 6));
      const boundaryOutput = Math.ceil(2 / step);
      expect(rendered[boundaryOutput]).not.toBe(rendered[boundaryOutput - 1]);
    },
  );

  it.each([48_000, 44_100])(
    'drains the last sample and does not carry a tail into the next utterance at %i Hz',
    (rate) => {
      const processor = createProcessor(rate);
      enqueue(processor, 0, [16_384]);
      const output = render(processor, 128);
      expect([...output.slice(0, 2)]).toEqual([0.5, 0.5]);
      expect([...output.slice(2)]).toEqual(new Array(126).fill(0));
      for (let i = 1; i < PCM_OUTPUT_REPORT_QUANTA; i += 1) render(processor, 128);
      expect(processor.port.messages).toContainEqual(
        expect.objectContaining({ consumedPackets: 1, consumedSamples: 1, queuedSamples: 0 }),
      );
      enqueue(processor, 0, [-16_384]);
      expect(render(processor, 1)[0]).toBe(-0.5);
    },
  );

  it('emits silence without flooding state messages when its queue is empty', () => {
    const processor = createProcessor(48_000);
    processor.port.send({ type: 'reset', epoch: 1 });

    for (let index = 0; index < PCM_OUTPUT_REPORT_QUANTA * 2; index += 1)
      expect([...render(processor, 128)]).toEqual(new Array(128).fill(0));

    expect(processor.port.messages).toEqual([]);
  });

  it('clears immediately on reset, ignores stale packets, and restarts in the new epoch', () => {
    const processor = createProcessor(24_000);
    processor.port.send({ type: 'reset', epoch: 1 });
    enqueue(processor, 1, [1_000, 2_000, 3_000]);
    expect(render(processor, 1)[0]).toBe(normalized(1_000));

    processor.port.send({ type: 'reset', epoch: 2 });
    enqueue(processor, 1, [20_000]);
    expect([...render(processor, 2)]).toEqual([0, 0]);
    enqueue(processor, 2, [-4_000, -5_000]);
    expect([...render(processor, 2)]).toEqual([-4_000, -5_000].map(normalized));
  });

  it('handles large fast arrivals with bounded aggregate consumption reports', () => {
    const processor = createProcessor(24_000, { maxQueuedPackets: 2_000 });
    processor.port.send({ type: 'reset', epoch: 3 });
    for (let index = 0; index < 1_500; index += 1) enqueue(processor, 3, [index]);

    const rendered: number[] = [];
    for (let quantum = 0; quantum < 12; quantum += 1) rendered.push(...render(processor, 128));
    expect(rendered.slice(0, 1_500)).toEqual(
      Array.from({ length: 1_500 }, (_, index) => normalized(index)),
    );
    expect(processor.port.messages.filter((message) => message.type === 'state')).toHaveLength(0);

    for (let quantum = 12; quantum < PCM_OUTPUT_REPORT_QUANTA; quantum += 1) render(processor, 128);
    expect(processor.port.messages).toEqual([
      {
        type: 'state',
        epoch: 3,
        consumedBytes: 3_000,
        consumedPackets: 1_500,
        consumedSamples: 1_500,
        queuedSamples: 0,
        underruns: 1,
      },
    ]);
    for (let quantum = 0; quantum < PCM_OUTPUT_REPORT_QUANTA; quantum += 1) render(processor, 128);
    expect(processor.port.messages).toHaveLength(1);
  });

  it.each([
    { maxQueuedSamples: 3, maxQueuedPackets: 4, packets: [[1, 2], [3], [4]] },
    { maxQueuedSamples: 10, maxQueuedPackets: 2, packets: [[1], [2], [3]] },
  ])('reports visible overflow without accepting data past either cap', (testCase) => {
    const processor = createProcessor(24_000, testCase);
    processor.port.send({ type: 'reset', epoch: 9 });
    testCase.packets.forEach((packet) => enqueue(processor, 9, packet));

    expect(processor.port.messages).toContainEqual({ type: 'overflow', epoch: 9 });
    const acceptedSamples = testCase.packets.slice(0, 2).flat();
    expect([...render(processor, acceptedSamples.length)]).toEqual(acceptedSamples.map(normalized));
  });
});
