import { APP_CONFIG } from "@/config/app-config";
import { cn } from "@/lib/utils";

/** Original HEaaN symbols, selected by the resolved theme before hydration. */
export function BrandLogo({ className, size = 40 }: { className?: string; size?: number }) {
  return (
    <span role="img" aria-label={APP_CONFIG.name} className={cn("inline-flex shrink-0", className)}>
      <img src="/brand/cryptolab-light.svg" alt="" width={size} height={size} className="block size-full dark:hidden" />
      <img src="/brand/cryptolab-dark.svg" alt="" width={size} height={size} className="hidden size-full dark:block" />
    </span>
  );
}
