import * as React from 'react';

import { cn } from '@/lib/utils';

// Switch is a button-shaped toggle with role="switch" + aria-checked.
// Keeps the bundle lean: zero radix dependency, ~150B gzipped vs the
// 2-3 KiB cost @radix-ui/react-switch would add. Mirrors shadcn/ui
// shape so the styling vocabulary stays consistent with Button / Badge.
export interface SwitchProps
  extends Omit<React.ButtonHTMLAttributes<HTMLButtonElement>, 'onChange'> {
  checked: boolean;
  onCheckedChange: (next: boolean) => void;
}

export const Switch = React.forwardRef<HTMLButtonElement, SwitchProps>(
  ({ checked, onCheckedChange, disabled, className, ...rest }, ref) => {
    return (
      <button
        ref={ref}
        type="button"
        role="switch"
        aria-checked={checked}
        disabled={disabled}
        onClick={() => onCheckedChange(!checked)}
        className={cn(
          'relative inline-flex h-5 w-9 shrink-0 cursor-pointer items-center rounded-full border-2 border-transparent transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 disabled:cursor-not-allowed disabled:opacity-50',
          checked ? 'bg-primary' : 'bg-muted',
          className,
        )}
        {...rest}
      >
        <span
          aria-hidden
          className={cn(
            'pointer-events-none inline-block h-4 w-4 transform rounded-full bg-background shadow ring-0 transition-transform',
            checked ? 'translate-x-4' : 'translate-x-0',
          )}
        />
      </button>
    );
  },
);
Switch.displayName = 'Switch';
