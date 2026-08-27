import type { Endpoints } from '../api/endpoints';

export class VoiceRecorder {
  private recorder: MediaRecorder | null = null;
  private stream: MediaStream | null = null;
  private chunks: Blob[] = [];

  async start(): Promise<void> {
    this.stream = await navigator.mediaDevices.getUserMedia({ audio: true });
    this.chunks = [];
    this.recorder = new MediaRecorder(this.stream);
    this.recorder.addEventListener('dataavailable', (event) => {
      if (event.data.size) this.chunks.push(event.data);
    });
    this.recorder.start();
  }

  async stop(endpoints: Endpoints): Promise<string> {
    const recorder = this.recorder;
    if (!recorder) return '';
    const blob = await new Promise<Blob>((resolve) => {
      recorder.addEventListener(
        'stop',
        () => resolve(new Blob(this.chunks, { type: recorder.mimeType || 'audio/webm' })),
        { once: true },
      );
      recorder.stop();
    });
    this.stream?.getTracks().forEach((track) => track.stop());
    this.recorder = null;
    this.stream = null;
    const data = new FormData();
    data.append('file', blob, 'recording.webm');
    const result = await endpoints.transcribe(data);
    return String(result.text || result.transcript || '');
  }

  cancel(): void {
    if (this.recorder?.state === 'recording') this.recorder.stop();
    this.stream?.getTracks().forEach((track) => track.stop());
    this.recorder = null;
    this.stream = null;
    this.chunks = [];
  }
}
