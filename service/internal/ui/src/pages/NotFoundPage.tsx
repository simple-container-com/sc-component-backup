import { Link } from "react-router-dom";
import { Compass } from "lucide-react";
import { Empty } from "@/components/ui/Empty";
import { useDocumentTitle } from "@/lib/hooks";

export function NotFoundPage() {
  useDocumentTitle("Not found");
  return (
    <Empty className="mt-16" icon={<Compass className="h-5 w-5" />} title="Page not found" action={<Link to="/" className="text-sm font-medium text-accent underline underline-offset-2">Back to overview</Link>}>
      The link may be outdated or the project was deleted.
    </Empty>
  );
}
