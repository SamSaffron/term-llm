export const TRANSCRIPT_SCROLL_TO_TAIL_EVENT = 'term-llm:scroll-transcript-to-tail';
export const TRANSCRIPT_SCROLL_TO_DURABLE_EVENT = 'term-llm:scroll-transcript-to-durable';

export function requestTranscriptScrollToTail(): void {
  document.getElementById('chatScroll')?.dispatchEvent(new Event(TRANSCRIPT_SCROLL_TO_TAIL_EVENT));
}

export function requestTranscriptScrollToDurable(id: number): void {
  document
    .getElementById('chatScroll')
    ?.dispatchEvent(new CustomEvent(TRANSCRIPT_SCROLL_TO_DURABLE_EVENT, { detail: id }));
}
