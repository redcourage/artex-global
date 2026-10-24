"use client";
import * as React from "react";

import { ArrowUpIcon, Loader2Icon, PaperclipIcon, SquareIcon, XIcon } from "lucide-react";
import { useTranslations } from "next-intl";

import { MentionTextarea } from "@/components/mention-textarea";
import { InputGroup, InputGroupAddon, InputGroupButton } from "@/components/ui/input-group";
import { shouldSubmitOnKey, useChatSendMode } from "@/lib/chat-send-mode";
import { isBtwCommand } from "@/lib/side-questions";
import type { ChatAttachment } from "@/lib/types";

function fmtBytes(n: number): string {
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)} MB`;
  if (n >= 1 << 10) return `${(n / (1 << 10)).toFixed(1)} KB`;
  return `${n} B`;
}
export type MainChatDraft = { text: string; attachments: ChatAttachment[]; uploading: boolean };
export const EMPTY_MAIN_DRAFT: MainChatDraft = { text: "", attachments: [], uploading: false };
export function MainChatComposer({
  input,
  attachments,
  uploading,
  mainBusy,
  sending,
  stopping,
  onChange,
  onRemove,
  onUpload,
  onSend,
  onStop,
  sideButton,
}: {
  input: string;
  attachments: ChatAttachment[];
  uploading: boolean;
  mainBusy: boolean;
  sending: boolean;
  stopping: boolean;
  onChange: (text: string) => void;
  onRemove: (path: string) => void;
  onUpload: (files: FileList | null) => Promise<void>;
  onSend: () => void;
  onStop: () => void;
  sideButton?: React.ReactNode;
}) {
  const t = useTranslations("sessions");
  const sendMode = useChatSendMode();
  const fileInputRef = React.useRef<HTMLInputElement>(null);
  return (
    <div className="shrink-0 border-t p-2">
      {attachments.length > 0 && (
        <div className="mb-2 flex flex-wrap gap-1.5">
          {attachments.map((a) => (
            <div
              key={a.path}
              className="flex items-center gap-1.5 rounded-md border bg-muted/50 px-2 py-1 text-xs"
              title={a.path}
            >
              <PaperclipIcon className="size-3 shrink-0 text-primary" />
              <span className="max-w-[160px] truncate">{a.name}</span>
              <span className="text-muted-foreground">{fmtBytes(a.size)}</span>
              <button
                type="button"
                className="ml-0.5 text-muted-foreground hover:text-foreground"
                onClick={() => onRemove(a.path)}
                title={t("composer.attachmentRemove")}
              >
                <XIcon className="size-3" />
              </button>
            </div>
          ))}
        </div>
      )}
      <input
        ref={fileInputRef}
        type="file"
        multiple
        className="hidden"
        onChange={(e) =>
          void onUpload(e.target.files).finally(() => {
            if (fileInputRef.current) fileInputRef.current.value = "";
          })
        }
      />
      <InputGroup className="min-h-9 has-disabled:opacity-100">
        <MentionTextarea
          inputGroup
          rows={1}
          aria-label={t("composer.mainAria")}
          placeholder={mainBusy ? t("composer.mainBusyPlaceholder") : t("composer.mainPlaceholder")}
          value={input}
          disabled={sending}
          onValueChange={onChange}
          onKeyDown={(e) => {
            if (!shouldSubmitOnKey(e, sendMode)) return;
            e.preventDefault();
            onSend();
          }}
          className="max-h-36 min-h-9 overflow-y-auto"
        />
        <InputGroupAddon align="block-end">
          {sideButton}
          <InputGroupButton
            size="icon-xs"
            variant="ghost"
            onClick={() => fileInputRef.current?.click()}
            disabled={mainBusy || uploading}
            title={t("composer.uploadTitle")}
            aria-label={t("composer.uploadTitle")}
          >
            {uploading ? <Loader2Icon className="animate-spin" /> : <PaperclipIcon />}
          </InputGroupButton>
          {mainBusy && isBtwCommand(input) && (
            <InputGroupButton size="icon-xs" onClick={onSend} aria-label={t("composer.sideQuestion")}>
              <ArrowUpIcon />
            </InputGroupButton>
          )}
          {mainBusy ? (
            <InputGroupButton
              className="ml-auto"
              size="icon-xs"
              variant="destructive"
              onClick={onStop}
              disabled={stopping}
              title={t("composer.stopTitle")}
              aria-label={t("composer.stopTitle")}
            >
              {stopping ? <Loader2Icon className="animate-spin" /> : <SquareIcon />}
            </InputGroupButton>
          ) : (
            <InputGroupButton
              className="ml-auto"
              size="icon-xs"
              variant="default"
              onClick={onSend}
              disabled={(!input.trim() && attachments.length === 0) || sending || uploading}
              title={t("composer.sendTitle")}
              aria-label={t("composer.sendTitle")}
            >
              {sending ? <Loader2Icon className="animate-spin" /> : <ArrowUpIcon />}
            </InputGroupButton>
          )}
        </InputGroupAddon>
      </InputGroup>
    </div>
  );
}
