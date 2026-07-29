import { redirect } from "@remix-run/react";

export const clientLoader = async () => {
  return redirect("/login");
};

export default function Index() {
  return null;
}