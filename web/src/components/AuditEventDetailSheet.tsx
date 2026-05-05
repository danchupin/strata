import { type AuditRecord } from '@/api/client';
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet';

interface AuditEventDetailSheetProps {
  record: AuditRecord | null;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

// AuditEventDetailSheet is the right-side detail panel surfaced when an
// operator clicks an audit row. Used by both the historical audit log
// viewer (US-018) and the live audit-tail page (US-002) so the schema and
// affordances stay in lockstep.
export function AuditEventDetailSheet({
  record,
  open,
  onOpenChange,
}: AuditEventDetailSheetProps) {
  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent
        side="right"
        className="w-full overflow-y-auto sm:max-w-lg"
        aria-describedby="audit-event-detail-desc"
      >
        <SheetHeader>
          <SheetTitle>Audit event</SheetTitle>
          <SheetDescription id="audit-event-detail-desc">
            Full row metadata. Click outside or press Esc to close.
          </SheetDescription>
        </SheetHeader>
        {record ? (
          <dl className="mt-6 space-y-3 text-sm">
            <DetailRow label="Time" value={new Date(record.time).toLocaleString()} mono />
            <DetailRow label="Action" value={record.action} mono />
            <DetailRow label="Result" value={record.result || '—'} mono />
            <DetailRow label="Principal" value={record.principal || '—'} mono />
            <DetailRow label="Bucket" value={record.bucket || '—'} mono />
            <DetailRow label="Resource" value={record.resource || '—'} mono wrap />
            <DetailRow
              label="Request ID"
              value={record.request_id || '—'}
              mono
            />
            <DetailRow label="Source IP" value={record.source_ip || '—'} mono />
            <DetailRow
              label="User-Agent"
              value={record.user_agent || '—'}
              mono
              wrap
            />
            <DetailRow label="Event ID" value={record.event_id || '—'} mono />
            <DetailRow label="Bucket ID" value={record.bucket_id || '—'} mono />
          </dl>
        ) : null}
      </SheetContent>
    </Sheet>
  );
}

function DetailRow({
  label,
  value,
  mono,
  wrap,
}: {
  label: string;
  value: string;
  mono?: boolean;
  wrap?: boolean;
}) {
  return (
    <div>
      <dt className="text-xs uppercase tracking-wide text-muted-foreground">
        {label}
      </dt>
      <dd
        className={[
          'mt-1 text-sm',
          mono ? 'font-mono text-xs' : '',
          wrap ? 'break-all' : 'truncate',
        ]
          .filter(Boolean)
          .join(' ')}
      >
        {value}
      </dd>
    </div>
  );
}
