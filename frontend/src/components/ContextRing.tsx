import type { ContextReading } from '../domain/context-meter';

/** A ring filled clockwise from twelve o'clock with the reading's share of the window. */
export function ContextRing({
  reading,
  size,
  stroke,
  label,
}: {
  reading: ContextReading;
  size: number;
  stroke: number;
  label?: string;
}) {
  const radius = (size - stroke) / 2;
  const circumference = 2 * Math.PI * radius;
  const center = size / 2;
  return (
    <svg
      class="context-ring"
      width={size}
      height={size}
      viewBox={`0 0 ${size} ${size}`}
      aria-hidden="true"
    >
      <circle class="context-ring-track" cx={center} cy={center} r={radius} stroke-width={stroke} />
      {reading.fill > 0 && (
        <circle
          class="context-ring-fill"
          cx={center}
          cy={center}
          r={radius}
          stroke-width={stroke}
          stroke-dasharray={`${(reading.fill * circumference).toFixed(2)} ${circumference.toFixed(2)}`}
          transform={`rotate(-90 ${center} ${center})`}
        />
      )}
      {label && (
        <text
          class="context-ring-label"
          x="50%"
          y="50%"
          text-anchor="middle"
          dominant-baseline="central"
        >
          {label}
        </text>
      )}
    </svg>
  );
}
