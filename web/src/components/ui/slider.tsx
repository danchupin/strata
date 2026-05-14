import * as React from 'react';

import { cn } from '@/lib/utils';

// Lightweight slider primitive wrapping <input type="range">. No third-party
// dep — keeps the bundle delta small (US-003 cluster-weights target ≤4 KiB
// gzipped for the whole pending-card + modal slice). The `value` prop is a
// number; consumers parse `e.target.value` themselves to keep `onChange`
// type-aligned with the underlying input.
export interface SliderProps
  extends Omit<React.InputHTMLAttributes<HTMLInputElement>, 'type'> {
  value: number;
}

const Slider = React.forwardRef<HTMLInputElement, SliderProps>(
  ({ className, value, ...props }, ref) => {
    return (
      <input
        ref={ref}
        type="range"
        value={value}
        className={cn(
          'h-2 w-full cursor-pointer appearance-none rounded-full bg-secondary accent-primary disabled:cursor-not-allowed disabled:opacity-50',
          className,
        )}
        {...props}
      />
    );
  },
);
Slider.displayName = 'Slider';

export { Slider };
